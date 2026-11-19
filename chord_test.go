package midi

import (
	"bytes"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/mattetti/filebuffer"
)

// fixedBytesTwoTrackChords is the hand computed binary representation of a
// Type 1 MIDI file with two tracks interleaving triads, sustain pedal hold
// and a velocity 0 note off. Overlapping note on FIFO pairing is covered by
// TestChordTimeline_OverlappingNoteOnFIFO. PPQ is 96.
//
// Track 0 (piano):
//
//	t=0   C4(60) on, G4(67) on
//	t=48  pedal down (CC64 127)
//	t=96  C4 off, G4 off (held by the pedal)
//	t=144 E4(64) on (vel 90), pedal up (CC64 0) -> C4/G4 end at 144
//	t=192 E4 off via note on velocity 0
//
// Track 1 (guitar):
//
//	t=0   E4(64) on
//	t=144 E4 off, A3(57) on
//	t=192 C4(60) on, E4(64) on
//	t=288 A3 off, C4 off, E4 off
var fixedBytesTwoTrackChords = []byte{
	// header chunk
	0x4d, 0x54, 0x68, 0x64, 0x00, 0x00, 0x00, 0x06,
	0x00, 0x01, // format 1
	0x00, 0x02, // 2 tracks
	0x00, 0x60, // 96 PPQ
	// track 0 chunk, size 44 (0x2c)
	0x4d, 0x54, 0x72, 0x6b,
	0x00, 0x00, 0x00, 0x2c,
	// hardcoded time signature prefix
	0x00, 0xff, 0x58, 0x04, 0x04, 0x02, 0x24, 0x08,
	0x00, 0x90, 60, 100, // t=0    C4 on v100
	0x00, 0x90, 67, 100, // t=0    G4 on v100
	0x30, 0xb0, 64, 127, // t=48   pedal down
	0x30, 0x80, 60, 64, // t=96   C4 off
	0x00, 0x80, 67, 64, // t=96   G4 off
	0x30, 0x90, 64, 90, // t=144  E4 on v90
	0x00, 0xb0, 64, 0, // t=144  pedal up
	0x30, 0x90, 64, 0, // t=192  E4 off (vel 0)
	0x00, 0xff, 0x2f, 0x00, // end of track
	// track 1 chunk, size 45 (0x2d)
	0x4d, 0x54, 0x72, 0x6b,
	0x00, 0x00, 0x00, 0x2d,
	// hardcoded time signature prefix
	0x00, 0xff, 0x58, 0x04, 0x04, 0x02, 0x24, 0x08,
	0x00, 0x90, 64, 80, // t=0    E4 on v80
	0x81, 0x10, 0x80, 64, 64, // t=144 E4 off (delta 144, 2 byte varint)
	0x00, 0x90, 57, 80, // t=144  A3 on
	0x30, 0x90, 60, 80, // t=192  C4 on
	0x00, 0x90, 64, 80, // t=192  E4 on
	0x60, 0x80, 57, 64, // t=288  A3 off (delta 96)
	0x00, 0x80, 60, 64, // t=288  C4 off
	0x00, 0x80, 64, 64, // t=288  E4 off
	0x00, 0xff, 0x2f, 0x00, // end of track
}

func decodeFixed(t *testing.T, data []byte) *Decoder {
	t.Helper()
	dec := NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(); err != nil {
		t.Fatalf("failed to decode fixed bytes: %v", err)
	}
	return dec
}

func TestChordTimeline_FixedBytesEncoding(t *testing.T) {
	// Recreate the fixed bytes with the encoder and verify the exact binary
	// representation, locking the codec behaviour the chord timeline relies on.
	buff := filebuffer.New(nil)
	defer buff.Close()
	enc := NewEncoder(buff, Syncronous, 96)

	tr0 := enc.NewTrack()
	tr0.AddAfterDelta(0, NoteOn(0, 60, 100))
	tr0.AddAfterDelta(0, NoteOn(0, 67, 100))
	tr0.AddAfterDelta(48, ControlChange(0, 64, 127))
	tr0.AddAfterDelta(48, NoteOff(0, 60))
	tr0.AddAfterDelta(0, NoteOff(0, 67))
	tr0.AddAfterDelta(48, NoteOn(0, 64, 90))
	tr0.AddAfterDelta(0, ControlChange(0, 64, 0))
	tr0.AddAfterDelta(48, NoteOn(0, 64, 0))

	tr1 := enc.NewTrack()
	tr1.AddAfterDelta(0, NoteOn(0, 64, 80))
	tr1.AddAfterDelta(144, NoteOff(0, 64))
	tr1.AddAfterDelta(0, NoteOn(0, 57, 80))
	tr1.AddAfterDelta(48, NoteOn(0, 60, 80))
	tr1.AddAfterDelta(0, NoteOn(0, 64, 80))
	tr1.AddAfterDelta(96, NoteOff(0, 57))
	tr1.AddAfterDelta(0, NoteOff(0, 60))
	tr1.AddAfterDelta(0, NoteOff(0, 64))

	if err := enc.Write(); err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if _, err := buff.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(buff)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fixedBytesTwoTrackChords) {
		t.Fatalf("encoded bytes don't match the fixed fixture\nwant %v\n got %v",
			fixedBytesTwoTrackChords, got)
	}

	// Round trip: decode the fixed bytes and re-encode all the channel voice
	// and control change events. The library's encoder doesn't support
	// re-serializing some meta events (e.g. the time signature prefix), so
	// those are skipped; the stream the chord timeline consumes is what is
	// exercised here.
	dec := decodeFixed(t, fixedBytesTwoTrackChords)
	buff2 := filebuffer.New(nil)
	defer buff2.Close()
	enc2 := NewEncoder(buff2, dec.Format, dec.TicksPerQuarterNote)
	for _, src := range dec.Tracks {
		dst := enc2.NewTrack()
		last := uint64(0)
		for _, ev := range src.Events {
			if ev.MsgType == EventByteMap["Meta"] && ev.Cmd != MetaByteMap["End of Track"] {
				continue
			}
			dst.AddAfterDelta(uint32(ev.AbsTicks-last), ev.Copy())
			last = ev.AbsTicks
		}
	}
	if err := enc2.Write(); err != nil {
		t.Fatalf("re-encode failed: %v", err)
	}
	if _, err := buff2.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	// Decoding the re-encoded stream must yield the same chord timeline.
	dec2 := NewDecoder(buff2)
	if err := dec2.Decode(); err != nil {
		t.Fatalf("round trip decode failed: %v", err)
	}
	roundTripChords, err := (&ChordExtractor{AttackToleranceTicks: 10}).Chords(dec2)
	if err != nil {
		t.Fatalf("round trip chord extraction failed: %v", err)
	}
	original, err := (&ChordExtractor{AttackToleranceTicks: 10}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTripChords) != len(original) {
		t.Fatalf("round trip produced %d chords, want %d", len(roundTripChords), len(original))
	}
	for i, c := range roundTripChords {
		w := original[i]
		if c.Start != w.Start || c.End != w.End || !reflect.DeepEqual(c.Notes, w.Notes) ||
			c.Quality != w.Quality || !reflect.DeepEqual(c.Sources, w.Sources) {
			t.Fatalf("round trip chord %d = %+v, want %+v", i, c, w)
		}
	}
}

func TestChordTimeline_FixedBytesChords(t *testing.T) {
	dec := decodeFixed(t, fixedBytesTwoTrackChords)
	ce := &ChordExtractor{AttackToleranceTicks: 10, FileName: "fixed.mid"}
	if src := ce.ChordSourceFile(); src != "fixed.mid" {
		t.Fatalf("source file = %q, want fixed.mid", src)
	}
	chords, err := ce.Chords(dec)
	if err != nil {
		t.Fatalf("chord extraction failed: %v", err)
	}

	want := ChordTimeline{
		{
			Start: 0, End: 144,
			Notes: []int{60, 64, 67},
			Root:  60, Quality: ChordQualityMajor, Name: "Cmaj",
			Sources: []ChordNoteSource{
				{Track: 0, Channel: 0, Key: 60, Start: 0, End: 144},
				{Track: 1, Channel: 0, Key: 64, Start: 0, End: 144},
				{Track: 0, Channel: 0, Key: 67, Start: 0, End: 144},
			},
		},
		{
			// E4 (track 0) attacks at 144 while A3 (track 1) keeps sounding
			// from 144. Both attacks are on tick 144, so they cluster.
			Start: 144, End: 192,
			Notes: []int{57, 64},
			Root:  57, Quality: ChordQualityUnknown, Name: "A?",
			Sources: []ChordNoteSource{
				{Track: 0, Channel: 0, Key: 64, Start: 144, End: 192},
				{Track: 1, Channel: 0, Key: 57, Start: 144, End: 288},
			},
		},
		{
			// C4/E4 attack at 192; A3 is still held but its attack was at 144
			// (48 ticks away, past the tolerance), so it is not part of this
			// group even though it overlaps in time.
			Start: 192, End: 288,
			Notes: []int{60, 64},
			Root:  60, Quality: ChordQualityUnknown, Name: "C?",
			Sources: []ChordNoteSource{
				{Track: 1, Channel: 0, Key: 60, Start: 192, End: 288},
				{Track: 1, Channel: 0, Key: 64, Start: 192, End: 288},
			},
		},
	}

	if len(chords) != len(want) {
		t.Fatalf("got %d chords, want %d: %+v", len(chords), len(want), chords)
	}
	for i, c := range chords {
		w := want[i]
		if c.Start != w.Start || c.End != w.End {
			t.Errorf("chord %d interval = [%d,%d], want [%d,%d]", i, c.Start, c.End, w.Start, w.End)
		}
		if !reflect.DeepEqual(c.Notes, w.Notes) {
			t.Errorf("chord %d notes = %v, want %v", i, c.Notes, w.Notes)
		}
		if c.Root != w.Root || c.Quality != w.Quality || c.Name != w.Name {
			t.Errorf("chord %d = root %d %s (%s), want root %d %s (%s)",
				i, c.Root, c.Name, c.Quality, w.Root, w.Name, w.Quality)
		}
		if !reflect.DeepEqual(c.Sources, w.Sources) {
			t.Errorf("chord %d sources = %+v, want %+v", i, c.Sources, w.Sources)
		}
	}

	// With a zero tolerance, the E4 attack at tick 0 (track 1) still joins the
	// tick 0 group, but the E4 attack at tick 144 starts its own group and the
	// C/G pedal-held notes are already gone. Verify tolerance actually changes
	// the grouping by pushing an attack past the tolerance window.
	strict, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if strict[0].Start != 0 || len(strict[0].Notes) != 3 {
		t.Fatalf("strict chord 0 = %+v, want the full Cmaj at tick 0", strict[0])
	}
}

func TestChordTimeline_ToleranceClustering(t *testing.T) {
	// C4 at tick 0, E4 at tick 5, G4 at tick 20.
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 5, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 64, Velocity: 100},
		{AbsTicks: 20, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 67, Velocity: 100},
		{AbsTicks: 100, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
		{AbsTicks: 100, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 64},
		{AbsTicks: 100, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 67},
	}}
	dec := &Decoder{Format: Syncronous, Tracks: []*Track{tr}}

	within, err := (&ChordExtractor{AttackToleranceTicks: 20}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(within) != 1 || within[0].Name != "Cmaj" {
		t.Fatalf("tolerance 20: got %d chords (%+v), want a single Cmaj", len(within), within)
	}

	split, err := (&ChordExtractor{AttackToleranceTicks: 10}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	// C4+E4 cluster (interval 5 <= 10), then G4 alone (20 > 10 from tick 0).
	if len(split) != 2 {
		t.Fatalf("tolerance 10: got %d chords, want 2", len(split))
	}
	if !reflect.DeepEqual(split[0].Notes, []int{60, 64}) || split[0].Quality != ChordQualityUnknown {
		t.Errorf("first group = %+v, want [C4 E4] unknown", split[0])
	}
	if !reflect.DeepEqual(split[1].Notes, []int{67}) {
		t.Errorf("second group = %+v, want [G4]", split[1])
	}

	exact, err := (&ChordExtractor{AttackToleranceTicks: 5}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 2 {
		t.Fatalf("tolerance 5 (boundary inclusive): got %d chords, want 2", len(exact))
	}
}

func TestChordTimeline_InvalidTolerance(t *testing.T) {
	dec := &Decoder{Format: Syncronous, Tracks: []*Track{{}}}
	if _, err := (&ChordExtractor{AttackToleranceTicks: -1}).Chords(dec); err != ErrInvalidTolerance {
		t.Fatalf("got %v, want ErrInvalidTolerance", err)
	}
}

func TestChordTimeline_Type2Rejected(t *testing.T) {
	dec := &Decoder{Format: Asyncronous, Tracks: []*Track{{}}}
	if _, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec); err != ErrType2NotSupported {
		t.Fatalf("got %v, want ErrType2NotSupported", err)
	}
}

func TestChordTimeline_EmptyAndNil(t *testing.T) {
	// No tracks at all: empty timeline, no error, no chord emitted.
	dec := &Decoder{Format: Syncronous}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 0 {
		t.Fatalf("got %d chords, want none", len(chords))
	}

	// Track with no note events.
	dec2 := &Decoder{Format: SingleTrack, Tracks: []*Track{{Events: []*Event{}}}}
	chords, err = dec2.ChordTimeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 0 {
		t.Fatalf("got %d chords, want none", len(chords))
	}

	// Nil inputs.
	if _, err := (*ChordExtractor)(nil).Chords(dec); err == nil {
		t.Fatal("nil extractor should error")
	}
	if _, err := (&ChordExtractor{}).Chords(nil); err == nil {
		t.Fatal("nil decoder should error")
	}
}

func TestChordTimeline_DrumChannel(t *testing.T) {
	// C4 on the drum channel (9) plus E4/G4 on a melodic channel.
	mk := func() *Decoder {
		tr := &Track{Events: []*Event{
			{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: DrumChannel, Note: 36, Velocity: 100},
			{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 64, Velocity: 100},
			{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 67, Velocity: 100},
			{AbsTicks: 50, MsgType: EventByteMap["NoteOff"], MsgChan: DrumChannel, Note: 36},
			{AbsTicks: 50, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 64},
			{AbsTicks: 50, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 67},
		}}
		return &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	}

	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(mk())
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 1 || reflect.DeepEqual(chords[0].Notes, []int{36, 64, 67}) {
		t.Fatalf("drum note should be excluded, got %+v", chords)
	}
	for _, s := range chords[0].Sources {
		if s.Channel == DrumChannel {
			t.Fatalf("drum source leaked into chord: %+v", s)
		}
	}

	withDrums, err := (&ChordExtractor{AttackToleranceTicks: 0, IncludeDrums: true}).Chords(mk())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withDrums[0].Notes, []int{36, 64, 67}) {
		t.Fatalf("drum note should be included, got %v", withDrums[0].Notes)
	}
}

func TestChordTimeline_MissingNoteOff(t *testing.T) {
	// C4 never gets a note off; it must be closed on the last event tick (96).
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 96, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 72, Velocity: 64}, // orphan off
	}}
	dec := &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 1 {
		t.Fatalf("got %d chords, want 1", len(chords))
	}
	if chords[0].Start != 0 || chords[0].End != 96 {
		t.Fatalf("unclosed note interval = [%d,%d], want [0,96]", chords[0].Start, chords[0].End)
	}
}

func TestChordTimeline_OverlappingNoteOnFIFO(t *testing.T) {
	// Two note ons for the same key, then two note offs: FIFO pairing means the
	// first on ends at the first off, the second on at the second off.
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 10, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 90},
		{AbsTicks: 40, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
		{AbsTicks: 80, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
	}}
	dec := &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 2 {
		t.Fatalf("got %d chords, want 2 (re-attack starts a new group)", len(chords))
	}
	if chords[0].Start != 0 || chords[0].End != 40 {
		t.Errorf("first interval = [%d,%d], want [0,40]", chords[0].Start, chords[0].End)
	}
	if chords[1].Start != 10 || chords[1].End != 80 {
		t.Errorf("second interval = [%d,%d], want [10,80]", chords[1].Start, chords[1].End)
	}
}

func TestChordTimeline_SustainPedal(t *testing.T) {
	// C4 released at 48 while pedal is down; pedal up at 96 extends it to 96.
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 10, MsgType: EventByteMap["ControlChange"], MsgChan: 0, Controller: 64, NewValue: 127},
		{AbsTicks: 48, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
		{AbsTicks: 96, MsgType: EventByteMap["ControlChange"], MsgChan: 0, Controller: 64, NewValue: 0},
	}}
	dec := &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 1 || chords[0].End != 96 {
		t.Fatalf("sustained note should end on pedal up at 96, got %+v", chords)
	}
}

func TestChordTimeline_SustainPedalFIFO(t *testing.T) {
	// Overlapping note ons under a held pedal, followed by two note offs and a
	// pedal up. Both notes survive until pedal up at 120, each carrying its
	// own release tick.
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["ControlChange"], MsgChan: 0, Controller: 64, NewValue: 127},
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 10, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 90},
		{AbsTicks: 40, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60}, // releases the first note
		{AbsTicks: 60, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60}, // releases the second
		{AbsTicks: 120, MsgType: EventByteMap["ControlChange"], MsgChan: 0, Controller: 64, NewValue: 0},
	}}
	dec := &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 2 {
		t.Fatalf("got %d chords, want 2 (two attacks)", len(chords))
	}
	for i, c := range chords {
		if c.End != 120 {
			t.Errorf("chord %d end = %d, want pedal up tick 120", i, c.End)
		}
	}
	if chords[0].Start != 0 || chords[1].Start != 10 {
		t.Fatalf("attacks = %d and %d, want 0 and 10", chords[0].Start, chords[1].Start)
	}
}

func TestChordTimeline_KeySignature(t *testing.T) {
	// First chord at tick 0 (no signature yet), G major (1 sharp) starts at
	// tick 30, second chord at tick 60.
	keySig := &Event{AbsTicks: 30, MsgType: EventByteMap["Meta"], MsgChan: 0xF, Cmd: MetaByteMap["Key Signature"], Key: 1, Scale: 0}
	tr := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 64, Velocity: 100},
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 67, Velocity: 100},
		keySig,
		{AbsTicks: 60, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
		{AbsTicks: 60, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 64},
		{AbsTicks: 60, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 67},
		{AbsTicks: 60, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 59, Velocity: 100},
		{AbsTicks: 60, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 62, Velocity: 100},
		{AbsTicks: 60, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 67, Velocity: 100},
		{AbsTicks: 90, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 59},
		{AbsTicks: 90, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 62},
		{AbsTicks: 90, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 67},
	}}
	dec := &Decoder{Format: SingleTrack, Tracks: []*Track{tr}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 2 {
		t.Fatalf("got %d chords, want 2", len(chords))
	}
	// No key signature before tick 0: implicit C major.
	if chords[0].HasKeySignature || chords[0].Key != 0 || chords[0].Scale != 0 {
		t.Fatalf("first chord key signature = %+v, want implicit C major", chords[0])
	}
	// The mid file change to G major applies to everything at or after tick 30.
	if !chords[1].HasKeySignature || chords[1].Key != 1 || chords[1].Scale != 0 {
		t.Fatalf("second chord key signature = %+v, want G major (key=1, major)", chords[1])
	}
	if chords[1].Name != "Gmaj" {
		t.Fatalf("second chord = %s, want Gmaj", chords[1].Name)
	}
}

func TestChordTimeline_Qualities(t *testing.T) {
	tests := []struct {
		name    string
		notes   []int
		quality ChordQuality
		root    int
		label   string
	}{
		{"major", []int{60, 64, 67}, ChordQualityMajor, 60, "Cmaj"},
		{"minor", []int{57, 60, 64}, ChordQualityMinor, 57, "Amin"},
		{"diminished", []int{60, 63, 66}, ChordQualityDiminished, 60, "Cdim"},
		{"augmented", []int{60, 64, 68}, ChordQualityAugmented, 60, "Caug"},
		{"sus4", []int{60, 65, 67}, ChordQualitySuspended4, 60, "Csus4"},
		{"sus2", []int{60, 62, 67}, ChordQualitySuspended2, 60, "Csus2"},
		{"major with extra note", []int{60, 64, 67, 71}, ChordQualityMajor, 60, "Cmaj"},
		{"first inversion: root found from lowest matching pitch class", []int{64, 67, 72}, ChordQualityMajor, 72, "Cmaj"},
		{"root position lowest note wins", []int{60, 64, 67}, ChordQualityMajor, 60, "Cmaj"},
		{"unmatched", []int{60, 61}, ChordQualityUnknown, 60, "C?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []*Event{}
			for _, n := range tt.notes {
				events = append(events, &Event{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: uint8(n), Velocity: 100})
			}
			for _, n := range tt.notes {
				events = append(events, &Event{AbsTicks: 50, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: uint8(n)})
			}
			dec := &Decoder{Format: SingleTrack, Tracks: []*Track{{Events: events}}}
			chords, err := (&ChordExtractor{AttackToleranceTicks: 0}).Chords(dec)
			if err != nil {
				t.Fatal(err)
			}
			if len(chords) != 1 {
				t.Fatalf("got %d chords, want 1", len(chords))
			}
			c := chords[0]
			if c.Quality != tt.quality || c.Root != tt.root || c.Name != tt.label {
				t.Fatalf("got root=%d quality=%s name=%s, want root=%d quality=%s name=%s",
					c.Root, c.Quality, c.Name, tt.root, tt.quality, tt.label)
			}
		})
	}
}

func TestChordTimeline_CrossTrackSameNote(t *testing.T) {
	// The same pitch from two tracks is deduplicated in Notes but keeps both
	// source occurrences.
	tr0 := &Track{Events: []*Event{
		{AbsTicks: 0, MsgType: EventByteMap["NoteOn"], MsgChan: 0, Note: 60, Velocity: 100},
		{AbsTicks: 40, MsgType: EventByteMap["NoteOff"], MsgChan: 0, Note: 60},
	}}
	tr1 := &Track{Events: []*Event{
		{AbsTicks: 2, MsgType: EventByteMap["NoteOn"], MsgChan: 1, Note: 60, Velocity: 80},
		{AbsTicks: 40, MsgType: EventByteMap["NoteOff"], MsgChan: 1, Note: 60},
	}}
	dec := &Decoder{Format: Syncronous, Tracks: []*Track{tr0, tr1}}
	chords, err := (&ChordExtractor{AttackToleranceTicks: 5}).Chords(dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(chords) != 1 {
		t.Fatalf("got %d chords, want 1", len(chords))
	}
	if !reflect.DeepEqual(chords[0].Notes, []int{60}) {
		t.Fatalf("pitch set = %v, want deduplicated [60]", chords[0].Notes)
	}
	if len(chords[0].Sources) != 2 {
		t.Fatalf("sources = %d, want both track occurrences", len(chords[0].Sources))
	}
	if chords[0].Sources[0].Track != 0 || chords[0].Sources[1].Track != 1 {
		t.Fatalf("source tracks = %+v, want track 0 then 1", chords[0].Sources)
	}
	if chords[0].Start != 0 || chords[0].End != 40 {
		t.Fatalf("interval = [%d,%d], want [0,40]", chords[0].Start, chords[0].End)
	}
}
