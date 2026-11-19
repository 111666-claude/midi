package midi

import (
	"bytes"
	"io/ioutil"
	"os"
	"reflect"
	"testing"

	"github.com/mattetti/filebuffer"
)

// buildMultiTrackFixture builds a Type 1, 96 ppq file exercising tempo
// changes, program changes, controllers, sustain and notes crossing the
// future segment boundaries.
//
// Tempo track (track 0):
//   - tempo 120 at tick 0, tempo 60 at tick 192
//
// Instrument track (track 1), channel 0:
//   - program 5 at tick 0
//   - CC7 volume 100 at tick 0, CC7 volume 110 at tick 96
//   - note 60: on 0    off 288  (crosses segment start)
//   - note 64: on 96   off 192  (ends exactly on segment start)
//   - note 67: on 192  off 384  (fully inside)
//   - note 72: on 288  (still ringing at segment end)
//   - sustain on (CC64=127) at tick 144, off (CC64=0) at tick 240
func buildMultiTrackFixture(t *testing.T) []byte {
	t.Helper()
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, Syncronous, 96)

	tempo := enc.NewTrack()
	tempo.SetName("Tempo")
	tempo.AddAfterDelta(0, TempoEvent(120))
	tempo.AddAfterDelta(192, TempoEvent(60))

	instr := enc.NewTrack()
	instr.SetName("Instr")
	instr.AddAfterDelta(0, ProgramChange(0, 5, 0))
	instr.AddAfterDelta(0, ControlChange(0, 7, 100))
	instr.AddAfterDelta(0, NoteOn(0, 60, 90))
	instr.AddAfterDelta(96, ControlChange(0, 7, 110))
	instr.AddAfterDelta(0, NoteOn(0, 64, 80))
	instr.AddAfterDelta(48, ControlChange(0, 64, 127))
	instr.AddAfterDelta(48, NoteOff(0, 64))
	instr.AddAfterDelta(0, NoteOn(0, 67, 70))
	instr.AddAfterDelta(48, ControlChange(0, 64, 0))
	instr.AddAfterDelta(48, NoteOff(0, 60))
	instr.AddAfterDelta(0, NoteOn(0, 72, 75))
	instr.AddAfterDelta(96, NoteOff(0, 67))

	if err := enc.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf.Bytes()...)
}

func decodeBytes(t *testing.T, data []byte) *Decoder {
	t.Helper()
	dec := NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(); err != nil {
		t.Fatal(err)
	}
	return dec
}

// mustWrite encodes into a filebuffer and returns its bytes, rewinding the
// buffer position first since Write leaves it at the end.
func mustWrite(t *testing.T, enc *Encoder, buf *filebuffer.Buffer) []byte {
	t.Helper()
	if err := enc.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf.Bytes()...)
}

func findEvent(tr *Track, pred func(*Event) bool) *Event {
	for _, ev := range tr.Events {
		if pred(ev) {
			return ev
		}
	}
	return nil
}

func TestExtractSegment_StartStateAndBoundaries(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 192, End: 480})
	if err != nil {
		t.Fatal(err)
	}
	if seg.Format != Syncronous || seg.TicksPerQuarterNote != 96 {
		t.Fatalf("header not preserved: format=%d ppq=%d", seg.Format, seg.TicksPerQuarterNote)
	}
	if len(seg.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(seg.Tracks))
	}

	instr := seg.Tracks[1]
	// Every output event must start at or after tick zero and be re-based.
	var maxTick uint64
	for _, ev := range instr.Events {
		if ev.AbsTicks > maxTick {
			maxTick = ev.AbsTicks
		}
	}
	if maxTick != 480-192 {
		t.Fatalf("expected last tick %d, got %d", 480-192, maxTick)
	}

	// Tempo snapshot: the tempo in effect at tick 192 is 60bpm.
	tempoTr := seg.Tracks[0]
	tempoEv := findEvent(tempoTr, func(e *Event) bool {
		return e.MsgType == EventByteMap["Meta"] && e.Cmd == MetaByteMap["Tempo"]
	})
	if tempoEv == nil || tempoEv.AbsTicks != 0 {
		t.Fatalf("expected tempo snapshot at tick 0, got %+v", tempoEv)
	}
	if tempoEv.Bpm != 60 {
		t.Fatalf("expected 60 bpm snapshot, got %d", tempoEv.Bpm)
	}
	// The earlier 120bpm tempo must not be carried over.
	count := 0
	for _, ev := range tempoTr.Events {
		if ev.MsgType == EventByteMap["Meta"] && ev.Cmd == MetaByteMap["Tempo"] {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 tempo event, got %d", count)
	}

	// Program snapshot at tick zero.
	pc := findEvent(instr, func(e *Event) bool { return e.MsgType == EventByteMap["ProgramChange"] })
	if pc == nil || pc.AbsTicks != 0 || pc.NewProgram != 5 {
		t.Fatalf("expected program 5 snapshot at tick 0, got %+v", pc)
	}

	// Controller snapshot: last CC7 before tick 192 is value 110.
	cc7 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["ControlChange"] && e.Controller == 7 && e.AbsTicks == 0
	})
	if cc7 == nil || cc7.NewValue != 110 {
		t.Fatalf("expected CC7=110 snapshot, got %+v", cc7)
	}

	// Cross-boundary note 60 (on at 0, off at 288) gets a synthesized onset
	// at tick zero.
	on60 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 60
	})
	if on60 == nil || on60.AbsTicks != 0 || on60.Velocity != 90 {
		t.Fatalf("expected synthesized NoteOn 60 at 0 with vel 90, got %+v", on60)
	}
	// Its original NoteOff at source tick 288 is re-based to 96 and kept.
	off60 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOff"] && e.Note == 60
	})
	if off60 == nil || off60.AbsTicks != 96 {
		t.Fatalf("expected NoteOff 60 at local tick 96, got %+v", off60)
	}

	// Note 64 ends exactly on start: its off is outside the half-open range,
	// but it was already closed, so it must not appear at all.
	if ev := findEvent(instr, func(e *Event) bool { return e.Note == 64 }); ev != nil {
		t.Fatalf("note 64 should have been excluded, got %+v", ev)
	}

	// Note 67 fully inside: onset re-based from 192 to 0, off from 384 to 192.
	on67 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 67
	})
	if on67 == nil || on67.AbsTicks != 0 {
		t.Fatalf("expected NoteOn 67 at local tick 0, got %+v", on67)
	}
	off67 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOff"] && e.Note == 67
	})
	if off67 == nil || off67.AbsTicks != 192 {
		t.Fatalf("expected NoteOff 67 at local tick 192, got %+v", off67)
	}

	// Note 72 opens inside and is still ringing at the end: synthesized
	// NoteOff on the last tick of the segment.
	on72 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 72
	})
	if on72 == nil || on72.AbsTicks != 96 {
		t.Fatalf("expected NoteOn 72 at local tick 96, got %+v", on72)
	}
	off72 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOff"] && e.Note == 72
	})
	if off72 == nil || off72.AbsTicks != 288 {
		t.Fatalf("expected synthesized NoteOff 72 at end tick 288, got %+v", off72)
	}

	// Sustain was held at the end (CC64 off at source 240 means released;
	// here it is released inside, so no end pedal event is required, but the
	// in-segment off event must be present).
	sustainOff := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["ControlChange"] && e.Controller == 64 && e.NewValue == 0
	})
	if sustainOff == nil {
		t.Fatal("expected the in-segment sustain off event to be kept")
	}
}

func TestExtractSegment_SustainHeldAtEnd(t *testing.T) {
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, Syncronous, 96)
	tr := enc.NewTrack()
	tr.AddAfterDelta(0, NoteOn(0, 60, 90))
	tr.AddAfterDelta(48, ControlChange(0, 64, 127)) // pedal down, never released
	dec := decodeBytes(t, mustWrite(t, enc, buf))

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 0, End: 192})
	if err != nil {
		t.Fatal(err)
	}
	events := seg.Tracks[0].Events
	// Ending order: sustain release before the final note off.
	var pedalRelease, finalOff *Event
	for _, ev := range events {
		if ev.MsgType == EventByteMap["ControlChange"] && ev.Controller == 64 && ev.NewValue == 0 && ev.AbsTicks == 192 {
			pedalRelease = ev
		}
		if ev.MsgType == EventByteMap["NoteOff"] && ev.Note == 60 && ev.AbsTicks == 192 {
			finalOff = ev
		}
	}
	if pedalRelease == nil {
		t.Fatal("expected a synthesized sustain release at the end tick")
	}
	if finalOff == nil {
		t.Fatal("expected a synthesized NoteOff at the end tick")
	}
	// Relative order at the end tick: pedal release comes first.
	idxPedal, idxOff := -1, -1
	for i, ev := range events {
		if ev == pedalRelease {
			idxPedal = i
		}
		if ev == finalOff {
			idxOff = i
		}
	}
	if idxPedal >= idxOff {
		t.Fatalf("pedal release (idx %d) must precede note off (idx %d)", idxPedal, idxOff)
	}
}

func TestExtractSegment_DropNotes(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	seg, err := ExtractSegment(dec, SegmentOptions{
		Start:              192,
		End:                480,
		CrossBoundaryNotes: DropNotes,
	})
	if err != nil {
		t.Fatal(err)
	}
	instr := seg.Tracks[1]

	// Note 60 crossed the start boundary: entirely dropped, no onset either.
	if ev := findEvent(instr, func(e *Event) bool { return e.Note == 60 }); ev != nil {
		t.Fatalf("crossing note 60 should be dropped, got %+v", ev)
	}
	// In-segment notes remain.
	if ev := findEvent(instr, func(e *Event) bool { return e.Note == 67 }); ev == nil {
		t.Fatal("note 67 fully inside should be kept")
	}
	if ev := findEvent(instr, func(e *Event) bool { return e.Note == 72 }); ev == nil {
		t.Fatal("note 72 opened inside should be kept")
	}
	// State snapshot is still emitted at zero.
	if pc := findEvent(instr, func(e *Event) bool { return e.MsgType == EventByteMap["ProgramChange"] }); pc == nil || pc.AbsTicks != 0 {
		t.Fatalf("program snapshot expected at tick 0, got %+v", pc)
	}
}

func TestExtractSegment_DropNotesOpenAtEnd(t *testing.T) {
	// A crossing note whose NoteOff is past the segment end must be dropped
	// without a dangling synthesized NoteOff.
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, SingleTrack, 96)
	tr := enc.NewTrack()
	tr.AddAfterDelta(0, NoteOn(0, 60, 90))   // crossing, off past the end
	tr.AddAfterDelta(100, NoteOn(0, 64, 80)) // fully inside
	tr.AddAfterDelta(100, NoteOff(0, 64))
	tr.AddAfterDelta(200, NoteOff(0, 60))
	dec := decodeBytes(t, mustWrite(t, enc, buf))

	seg, err := ExtractSegment(dec, SegmentOptions{
		Start:              50,
		End:                250,
		CrossBoundaryNotes: DropNotes,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range seg.Tracks[0].Events {
		if ev.Note == 60 {
			t.Fatalf("dropped crossing note 60 must not appear at all, got %+v", ev)
		}
	}
	// The surviving inside note must remain properly paired.
	on := findEvent(seg.Tracks[0], func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 64
	})
	off := findEvent(seg.Tracks[0], func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOff"] && e.Note == 64
	})
	if on == nil || off == nil || on.AbsTicks >= off.AbsTicks {
		t.Fatalf("inside note 64 not properly paired: on=%+v off=%+v", on, off)
	}
}

func TestExtractSegment_StartsAtZero(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 0, End: 192})
	if err != nil {
		t.Fatal(err)
	}
	instr := seg.Tracks[1]
	// Program 5 and CC7=100 are real events at source tick 0, they must be
	// preserved exactly (not duplicated, still at local tick 0).
	pcs := 0
	for _, ev := range instr.Events {
		if ev.MsgType == EventByteMap["ProgramChange"] {
			pcs++
			if ev.AbsTicks != 0 || ev.NewProgram != 5 {
				t.Fatalf("unexpected program event %+v", ev)
			}
		}
	}
	if pcs != 1 {
		t.Fatalf("expected 1 program change, got %d", pcs)
	}
	// Note 60 starts at 0 and extends past 192: synthesized end at tick 192.
	off60 := findEvent(instr, func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOff"] && e.Note == 60
	})
	if off60 == nil || off60.AbsTicks != 192 {
		t.Fatalf("expected NoteOff 60 synthesized at 192, got %+v", off60)
	}
}

func TestExtractSegment_SameTickOrderingAndSourceStability(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 192, End: 384})
	if err != nil {
		t.Fatal(err)
	}
	instr := seg.Tracks[1]

	// At local tick 0 the source had NoteOn 67 immediately after the boundary
	// events; collect the tick-zero events and verify the snapshot prefix is
	// ordered tempo/program/controller before any note event.
	var zeroEvents []*Event
	for _, ev := range instr.Events {
		if ev.AbsTicks == 0 {
			zeroEvents = append(zeroEvents, ev)
		}
	}
	if len(zeroEvents) < 3 {
		t.Fatalf("expected at least 3 tick-zero events, got %d", len(zeroEvents))
	}
	// Program change must come before the note onset.
	idxPC, idxNoteOn := -1, -1
	for i, ev := range zeroEvents {
		if ev.MsgType == EventByteMap["ProgramChange"] {
			idxPC = i
		}
		if ev.MsgType == EventByteMap["NoteOn"] {
			if idxNoteOn == -1 {
				idxNoteOn = i
			}
		}
	}
	if idxPC == -1 || idxNoteOn == -1 || idxPC > idxNoteOn {
		t.Fatalf("unexpected tick-zero ordering: pc=%d noteon=%d", idxPC, idxNoteOn)
	}

	// Source decoder must be untouched: original track still has its events
	// at their original absolute ticks.
	origInstr := dec.Tracks[1]
	firstOn := origInstr.Events[0]
	if firstOn.AbsTicks != 0 {
		t.Fatalf("source was modified, first event abs tick = %d", firstOn.AbsTicks)
	}
	// Re-decoding the source bytes yields the same event count.
	redec := decodeBytes(t, src)
	if len(redec.Tracks[1].Events) != len(origInstr.Events) {
		t.Fatal("source file content changed after extraction")
	}
}

func TestExtractSegment_RoundTrip(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 192, End: 480})
	if err != nil {
		t.Fatal(err)
	}

	// Encode to bytes and decode the result again.
	data, err := seg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	redec := decodeBytes(t, data)
	if redec.Format != Syncronous || redec.TicksPerQuarterNote != 96 {
		t.Fatalf("round trip header mismatch: %d / %d", redec.Format, redec.TicksPerQuarterNote)
	}
	if len(redec.Tracks) != 2 {
		t.Fatalf("round trip expected 2 tracks, got %d", len(redec.Tracks))
	}
	// Every decoded event must carry valid, monotonically non-decreasing ticks.
	for ti, tr := range redec.Tracks {
		var prev uint64
		for _, ev := range tr.Events {
			if ev.AbsTicks < prev {
				t.Fatalf("track %d: tick went backwards: %d after %d", ti, ev.AbsTicks, prev)
			}
			prev = ev.AbsTicks
		}
	}
	// Notes surviving the extraction must still be properly paired after the
	// round trip (each NoteOn has a matching NoteOff).
	for ti, tr := range redec.Tracks {
		open := map[noteKey]bool{}
		for _, ev := range tr.Events {
			key := noteKey{ev.MsgChan, ev.Note}
			switch {
			case ev.MsgType == EventByteMap["NoteOn"] && ev.Velocity > 0:
				if open[key] {
					t.Fatalf("track %d: note %+v re-opened without closing", ti, key)
				}
				open[key] = true
			case ev.MsgType == EventByteMap["NoteOff"] || (ev.MsgType == EventByteMap["NoteOn"] && ev.Velocity == 0):
				if !open[key] {
					t.Fatalf("track %d: unpaired note off for %+v after round trip", ti, key)
				}
				delete(open, key)
			}
		}
		if len(open) != 0 {
			t.Fatalf("track %d: unclosed notes after round trip: %v", ti, open)
		}
	}

	// EncodeTo via a seeker must produce the same bytes as Encode. The
	// comparison uses two freshly extracted segments because the existing
	// Track.ChunkData implementation mutates a track when it carries a name.
	// The filebuffer read index must be rewound before reading its bytes.
	seg2, err := ExtractSegment(decodeBytes(t, src), SegmentOptions{Start: 192, End: 480})
	if err != nil {
		t.Fatal(err)
	}
	buf2 := filebuffer.New(nil)
	if err := seg2.EncodeTo(buf2); err != nil {
		t.Fatal(err)
	}
	if _, err := buf2.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, buf2.Bytes()) {
		t.Fatal("Encode and EncodeTo produced different outputs")
	}
}

func TestExtractSegment_Type0(t *testing.T) {
	// Build a Type 0 file and extract from it.
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, SingleTrack, 96)
	tr := enc.NewTrack()
	tr.AddAfterDelta(0, TempoEvent(120))
	tr.AddAfterDelta(0, ProgramChange(0, 9, 0))
	tr.AddAfterDelta(0, NoteOn(0, 60, 90))
	tr.AddAfterDelta(96, NoteOn(1, 48, 80)) // second channel inside the track
	tr.AddAfterDelta(96, NoteOff(0, 60))
	tr.AddAfterDelta(96, NoteOff(1, 48))
	dec := decodeBytes(t, mustWrite(t, enc, buf))
	if dec.Format != SingleTrack {
		t.Fatalf("test setup: expected type 0, got %d", dec.Format)
	}

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 96, End: 288})
	if err != nil {
		t.Fatal(err)
	}
	if seg.Format != SingleTrack || len(seg.Tracks) != 1 {
		t.Fatalf("type 0 should stay single track: format=%d tracks=%d", seg.Format, len(seg.Tracks))
	}
	// Channel 1's note (on at 96) is inside; channel 0's note (on at 0)
	// crosses the boundary and gets truncated.
	events := seg.Tracks[0].Events
	on48 := findEvent(eventsAsTrack(events), func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 48
	})
	if on48 == nil || on48.AbsTicks != 0 || on48.MsgChan != 1 {
		t.Fatalf("expected channel 1 note 48 at local 0, got %+v", on48)
	}
	on60 := findEvent(eventsAsTrack(events), func(e *Event) bool {
		return e.MsgType == EventByteMap["NoteOn"] && e.Note == 60
	})
	if on60 == nil || on60.AbsTicks != 0 || on60.MsgChan != 0 {
		t.Fatalf("expected truncated channel 0 note 60 at local 0, got %+v", on60)
	}
}

// eventsAsTrack wraps a loose event slice so findEvent can be reused.
func eventsAsTrack(events []*Event) *Track {
	return &Track{Events: events}
}

func TestExtractSegment_InvalidInputs(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	tests := []struct {
		name string
		opts SegmentOptions
		want error
	}{
		{"negative start", SegmentOptions{Start: -1, End: 10}, ErrInvalidBoundaries},
		{"negative end", SegmentOptions{Start: 0, End: -10}, ErrInvalidBoundaries},
		{"reversed boundaries", SegmentOptions{Start: 100, End: 50}, ErrInvalidBoundaries},
		{"empty interval", SegmentOptions{Start: 100, End: 100}, ErrEmptySegment},
		{"start too large", SegmentOptions{Start: maxMIDITick + 1, End: maxMIDITick + 2}, ErrTickOutOfRange},
		{"end too large", SegmentOptions{Start: 0, End: maxMIDITick + 1}, ErrTickOutOfRange},
		{"length too large", SegmentOptions{Start: 1, End: maxMIDITick + 1}, ErrTickOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExtractSegment(dec, tt.opts)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errorsIs(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
		})
	}

	if _, err := ExtractSegment(nil, SegmentOptions{Start: 0, End: 10}); err == nil {
		t.Fatal("expected error for nil decoder")
	}
}

func TestExtractSegment_Type2Rejected(t *testing.T) {
	// A minimal in-memory type 2 header followed by an empty track.
	data := []byte{
		'M', 'T', 'h', 'd', 0, 0, 0, 6,
		0, 2, // format 2
		0, 1, // one track
		0, 96, // ppq
		'M', 'T', 'r', 'k', 0, 0, 0, 4,
		0, 0xFF, 0x2F, 0, // end of track
	}
	dec := decodeBytes(t, data)
	if _, err := ExtractSegment(dec, SegmentOptions{Start: 0, End: 10}); !errorsIs(err, ErrFmtNotSupported) {
		t.Fatalf("expected ErrFmtNotSupported for type 2, got %v", err)
	}
}

func TestExtractSegment_UnpairedNoteOff(t *testing.T) {
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, SingleTrack, 96)
	tr := enc.NewTrack()
	// Note off with no preceding note on.
	tr.AddAfterDelta(48, NoteOff(0, 60))
	dec := decodeBytes(t, mustWrite(t, enc, buf))
	_, err := ExtractSegment(dec, SegmentOptions{Start: 0, End: 192})
	if !errorsIs(err, ErrUnpairedNote) {
		t.Fatalf("expected ErrUnpairedNote, got %v", err)
	}
}

func TestExtractSegment_ControllerConflictLastWriteWins(t *testing.T) {
	buf := filebuffer.New(nil)
	enc := NewEncoder(buf, SingleTrack, 96)
	tr := enc.NewTrack()
	tr.AddAfterDelta(0, ControlChange(0, 7, 50))
	tr.AddAfterDelta(24, ControlChange(0, 7, 80))
	tr.AddAfterDelta(24, ControlChange(0, 7, 120))
	tr.AddAfterDelta(48, NoteOn(0, 60, 90))
	dec := decodeBytes(t, mustWrite(t, enc, buf))

	seg, err := ExtractSegment(dec, SegmentOptions{Start: 100, End: 200})
	if err != nil {
		t.Fatal(err)
	}
	snap := findEvent(seg.Tracks[0], func(e *Event) bool {
		return e.MsgType == EventByteMap["ControlChange"] && e.Controller == 7
	})
	if snap == nil || snap.NewValue != 120 || snap.AbsTicks != 0 {
		t.Fatalf("expected single CC7=120 snapshot at 0, got %+v", snap)
	}
}

func TestWriteSegment_ToFile(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)

	f, err := ioutil.TempFile("", "midi-segment-*.mid")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := WriteSegment(f, dec, SegmentOptions{Start: 192, End: 480}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	redec := NewDecoder(f)
	if err := redec.Decode(); err != nil {
		t.Fatal(err)
	}
	if len(redec.Tracks) != 2 {
		t.Fatalf("expected 2 tracks in written file, got %d", len(redec.Tracks))
	}
}

// errorsIs is a thin local wrapper keeping the test compatible with older Go
// versions while still unwrapping wrapped sentinel errors.
func errorsIs(err, target error) bool {
	if err == nil || target == nil {
		return err == target
	}
	if err == target {
		return true
	}
	type wrapper interface{ Unwrap() error }
	for {
		w, ok := err.(wrapper)
		if !ok {
			return false
		}
		err = w.Unwrap()
		if err == target {
			return true
		}
		if err == nil {
			return false
		}
	}
}

func TestExtractSegment_DeltaTimeRebasing(t *testing.T) {
	src := buildMultiTrackFixture(t)
	dec := decodeBytes(t, src)
	seg, err := ExtractSegment(dec, SegmentOptions{Start: 192, End: 480})
	if err != nil {
		t.Fatal(err)
	}
	// Recomputed delta times must reconstruct each event's absolute tick and
	// never exceed the segment length (a track can end before the segment end).
	for _, tr := range seg.Tracks {
		var sum uint64
		for _, ev := range tr.Events {
			sum += uint64(ev.TimeDelta)
			if sum != ev.AbsTicks {
				t.Fatalf("track %q: delta sum %d doesn't match abs tick %d", tr.Name(), sum, ev.AbsTicks)
			}
			if ev.AbsTicks > 480-192 {
				t.Fatalf("track %q: event past segment end at tick %d", tr.Name(), ev.AbsTicks)
			}
		}
	}
}

func TestExtractSegment_KeepsFixtureReadable(t *testing.T) {
	// Sanity check against a shipped fixture: extraction must succeed and the
	// source must still decode identically afterwards.
	for _, path := range []string{"fixtures/example-format0.mid", "fixtures/example-format1.mid"} {
		path := path
		t.Run(path, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			orig, err := ioutil.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			dec := decodeBytes(t, orig)
			lastTick := uint64(0)
			for _, tr := range dec.Tracks {
				for _, ev := range tr.Events {
					if ev.AbsTicks > lastTick {
						lastTick = ev.AbsTicks
					}
				}
			}
			if lastTick < 4 {
				t.Skip("fixture too short for a mid segment")
			}
			mid := int64(lastTick / 2)
			seg, err := ExtractSegment(dec, SegmentOptions{Start: 0, End: mid})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seg.Encode(); err != nil {
				t.Fatal(err)
			}
			// Source bytes unchanged on disk.
			f2, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			again, err := ioutil.ReadAll(f2)
			f2.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(orig, again) {
				t.Fatal("source fixture changed on disk")
			}
		})
	}
}
