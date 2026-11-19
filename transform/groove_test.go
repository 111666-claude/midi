package transform

import (
	"math"
	"reflect"
	"testing"

	"github.com/go-audio/midi"
	"github.com/mattetti/filebuffer"
)

const (
	groovePPQ   = 96
	grooveBar   = 4 * groovePPQ
	groove16th  = groovePPQ / 4
	kick, snare = 36, 38
	hat         = 42
)

// playedDrums returns a two-bar reference drum pattern played with feel:
// kicks (beats 1 & 3) laid back +6 ticks, snares (beats 2 & 4) rushing
// -8 ticks, hats on the off 8ths dragged +10 ticks. Velocities are accented
// per instrument.
func playedDrums() midi.AbsEvents {
	evs := midi.AbsEvents{}
	for bar := 0; bar < 2; bar++ {
		base := bar * grooveBar
		// kicks on beats 1 and 3, late, accented
		for _, beat := range []int{0, 2 * groovePPQ} {
			evs = append(evs, &midi.AbsEv{Start: base + beat + 6, Duration: groove16th, Vel: 100, MIDINote: kick})
		}
		// snares on beats 2 and 4, early
		for _, beat := range []int{groovePPQ, 3 * groovePPQ} {
			evs = append(evs, &midi.AbsEv{Start: base + beat - 8, Duration: groove16th, Vel: 90, MIDINote: snare})
		}
		// hats on the off 8ths, dragged
		for _, off := range []int{groovePPQ / 2, 3 * groovePPQ / 2, 5 * groovePPQ / 2, 7 * groovePPQ / 2} {
			evs = append(evs, &midi.AbsEv{Start: base + off + 10, Duration: groove16th, Vel: 60, MIDINote: hat})
		}
	}
	return evs
}

// quantizedDrums returns the same pattern as playedDrums but perfectly on
// the grid with a flat velocity, as a stiff quantized recording would.
func quantizedDrums() midi.AbsEvents {
	evs := midi.AbsEvents{}
	for bar := 0; bar < 2; bar++ {
		base := bar * grooveBar
		for _, beat := range []int{0, 2 * groovePPQ} {
			evs = append(evs, &midi.AbsEv{Start: base + beat, Duration: groove16th, Vel: 80, MIDINote: kick})
		}
		for _, beat := range []int{groovePPQ, 3 * groovePPQ} {
			evs = append(evs, &midi.AbsEv{Start: base + beat, Duration: groove16th, Vel: 80, MIDINote: snare})
		}
		for _, off := range []int{groovePPQ / 2, 3 * groovePPQ / 2, 5 * groovePPQ / 2, 7 * groovePPQ / 2} {
			evs = append(evs, &midi.AbsEv{Start: base + off, Duration: groove16th, Vel: 80, MIDINote: hat})
		}
	}
	return evs
}

// quantizedChords returns two bars of block chords, one chord per half bar,
// all notes starting on the same tick.
func quantizedChords() midi.AbsEvents {
	chords := [][3]int{{60, 64, 67}, {65, 69, 72}, {67, 71, 74}, {60, 64, 67}}
	evs := midi.AbsEvents{}
	for i, chord := range chords {
		start := i * 2 * groovePPQ
		for _, note := range chord {
			evs = append(evs, &midi.AbsEv{Start: start, Duration: groovePPQ, Vel: 80, MIDINote: note})
		}
	}
	return evs
}

// gridPositions maps each drum instrument to its on-grid positions inside a
// bar, used to verify how far notes moved off the grid.
var drumGridPositions = map[int][]int{
	kick:  {0, 2 * groovePPQ},
	snare: {groovePPQ, 3 * groovePPQ},
	hat:   {groovePPQ / 2, 3 * groovePPQ / 2, 5 * groovePPQ / 2, 7 * groovePPQ / 2},
}

// offsetFromGrid returns the signed distance of pos to one of the grid
// positions of the given instrument, or false if none matches.
func offsetFromGrid(note, pos int) (int, bool) {
	pos = ((pos % grooveBar) + grooveBar) % grooveBar
	for _, g := range drumGridPositions[note] {
		if off := pos - g; off > -groove16th && off < groove16th {
			return off, true
		}
	}
	return 0, false
}

func mustExtract(t *testing.T, ref midi.AbsEvents) GrooveTemplate {
	t.Helper()
	tmpl, err := ExtractGroove(ref, groovePPQ, 0)
	if err != nil {
		t.Fatalf("ExtractGroove() unexpected error: %v", err)
	}
	return tmpl
}

func TestExtractGroove_TimingAndAccents(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())

	if tmpl.Cycle != grooveBar {
		t.Fatalf("expected cycle of %d ticks, got %d", grooveBar, tmpl.Cycle)
	}
	// 8 distinct grid positions per bar
	if len(tmpl.Points) != 8 {
		t.Fatalf("expected 8 groove points, got %d: %+v", len(tmpl.Points), tmpl.Points)
	}
	wantOffsets := map[int]int{
		0:                 6,  // kick, laid back
		groovePPQ / 2:     10, // hat, dragged
		groovePPQ:         -8, // snare, rushing
		3 * groovePPQ / 2: 10, // hat
		2 * groovePPQ:     6,  // kick
		5 * groovePPQ / 2: 10, // hat
		3 * groovePPQ:     -8, // snare
		7 * groovePPQ / 2: 10, // hat
	}
	for _, p := range tmpl.Points {
		want, ok := wantOffsets[p.Position]
		if !ok {
			t.Fatalf("unexpected groove point at position %d", p.Position)
		}
		if p.Offset != want {
			t.Errorf("position %d: expected offset %d, got %d", p.Position, want, p.Offset)
		}
	}
	// accents: kicks are louder than average, hats softer
	avgVel := float64(2*100+2*90+4*60) / 8
	for _, p := range tmpl.Points {
		var wantRatio float64
		switch p.Position {
		case 0, 2 * groovePPQ:
			wantRatio = 100 / avgVel
		case groovePPQ, 3 * groovePPQ:
			wantRatio = 90 / avgVel
		default:
			wantRatio = 60 / avgVel
		}
		if diff := p.VelocityRatio - wantRatio; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("position %d: expected velocity ratio %v, got %v", p.Position, wantRatio, p.VelocityRatio)
		}
	}
}

func TestExtractGroove_Errors(t *testing.T) {
	if _, err := ExtractGroove(playedDrums(), 0, 0); err != ErrInvalidPPQ {
		t.Errorf("expected ErrInvalidPPQ, got %v", err)
	}
	if _, err := ExtractGroove(midi.AbsEvents{}, groovePPQ, 0); err != ErrEmptyReference {
		t.Errorf("expected ErrEmptyReference, got %v", err)
	}
	if _, err := ExtractGroove(nil, groovePPQ, 0); err != ErrEmptyReference {
		t.Errorf("expected ErrEmptyReference for nil reference, got %v", err)
	}
	neg := midi.AbsEvents{{Start: -10, Duration: 10, Vel: 80, MIDINote: kick}}
	if _, err := ExtractGroove(neg, groovePPQ, 0); err == nil {
		t.Error("expected an error for a negative onset, got nil")
	}
	for _, vel := range []int{0, 128, -5} {
		bad := midi.AbsEvents{{Start: 0, Duration: 10, Vel: vel, MIDINote: kick}}
		if _, err := ExtractGroove(bad, groovePPQ, 0); err == nil {
			t.Errorf("expected an error for velocity %d, got nil", vel)
		}
	}
}

func TestGrooveApply_AheadBehindAndAccents(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := quantizedDrums()
	got, err := tmpl.Apply(in, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("expected %d events, got %d", len(in), len(got))
	}
	avgVel := float64(2*100+2*90+4*60) / 8
	for _, ev := range got {
		var wantOffset int
		var wantVel int
		switch ev.MIDINote {
		case kick:
			wantOffset, wantVel = 6, int(80*100/avgVel+0.5)
		case snare:
			wantOffset, wantVel = -8, int(80*90/avgVel+0.5)
		case hat:
			wantOffset, wantVel = 10, int(80*60/avgVel+0.5)
		}
		off, ok := offsetFromGrid(ev.MIDINote, ev.Start)
		if !ok {
			t.Errorf("note %d at %d: not near any of its grid positions", ev.MIDINote, ev.Start)
			continue
		}
		if off != wantOffset {
			t.Errorf("note %d at %d: expected offset %d from grid, got %d", ev.MIDINote, ev.Start, wantOffset, off)
		}
		if ev.Vel != wantVel {
			t.Errorf("note %d at %d: expected velocity %d, got %d", ev.MIDINote, ev.Start, wantVel, ev.Vel)
		}
		if ev.Duration != groove16th {
			t.Errorf("note %d at %d: expected duration preserved at %d, got %d", ev.MIDINote, ev.Start, groove16th, ev.Duration)
		}
	}
}

func TestGrooveApply_StrengthZeroIsIdentity(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := append(quantizedDrums(), quantizedChords()...)
	got, err := tmpl.Apply(in, 0.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("strength 0 should be the identity\ngot  %+v\nwant %+v", got, in)
	}
	// the input must not have been mutated
	for i, ev := range in {
		if got[i] == ev {
			t.Errorf("event %d: expected a copy, got the same pointer", i)
		}
	}
}

func TestGrooveApply_HalfStrength(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	got, err := tmpl.Apply(quantizedDrums(), 0.5, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	for _, ev := range got {
		var wantOffset int
		switch ev.MIDINote {
		case kick:
			wantOffset = 3
		case snare:
			wantOffset = -4
		case hat:
			wantOffset = 5
		}
		off, ok := offsetFromGrid(ev.MIDINote, ev.Start)
		if !ok {
			t.Errorf("note %d at %d: not near any of its grid positions", ev.MIDINote, ev.Start)
			continue
		}
		if off != wantOffset {
			t.Errorf("note %d at %d: expected half offset %d, got %d", ev.MIDINote, ev.Start, wantOffset, off)
		}
	}
}

func TestGrooveApply_MoveNoteOff(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := quantizedDrums()

	moved, err := tmpl.Apply(in, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	// the output is re-sorted, so compare multisets keyed by note
	wantDur := map[int][]int{}
	for _, ev := range in {
		wantDur[ev.MIDINote] = append(wantDur[ev.MIDINote], ev.Duration)
	}
	for _, ev := range moved {
		durs := wantDur[ev.MIDINote]
		found := false
		for i, d := range durs {
			if d == ev.Duration {
				wantDur[ev.MIDINote] = append(durs[:i], durs[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			t.Errorf("moveNoteOff: note %d has unexpected duration %d", ev.MIDINote, ev.Duration)
		}
	}

	fixed, err := tmpl.Apply(in, 1.0, false)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	// with a fixed note off, the multiset of note ends must be preserved
	wantEnds := map[int]int{}
	for _, ev := range in {
		wantEnds[ev.End()]++
	}
	for _, ev := range fixed {
		wantEnds[ev.End()]--
	}
	for end, n := range wantEnds {
		if n != 0 {
			t.Errorf("fixed note off: end %d has a count mismatch of %d", end, n)
		}
	}
}

func TestGrooveApply_SameTickChordsAndBarBoundary(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := quantizedChords()
	// add a chord right before the bar boundary; its closest groove point is
	// the downbeat of the next cycle (wrap-around)
	boundary := midi.AbsEvents{
		{Start: grooveBar - 4, Duration: groovePPQ, Vel: 80, MIDINote: 60},
		{Start: grooveBar - 4, Duration: groovePPQ, Vel: 80, MIDINote: 64},
		{Start: grooveBar - 4, Duration: groovePPQ, Vel: 80, MIDINote: 67},
	}
	in = append(in, boundary...)

	got, err := tmpl.Apply(in, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("expected %d events, got %d", len(in), len(got))
	}
	// all notes of a chord must move together and stay grouped
	byStart := map[int][]*midi.AbsEv{}
	for _, ev := range got {
		byStart[ev.Start] = append(byStart[ev.Start], ev)
	}
	for start, chord := range byStart {
		if len(chord) != 3 {
			t.Errorf("expected 3 notes at start %d, got %d", start, len(chord))
		}
	}
	// the boundary chord sits 4 ticks before the downbeat whose offset is
	// +6, so it should land 6 ticks after its grid position, across the
	// bar line
	foundBoundary := false
	for _, ev := range got {
		if ev.MIDINote == 60 && ev.Start == grooveBar-4+6 {
			foundBoundary = true
		}
	}
	if !foundBoundary {
		t.Errorf("boundary chord: expected a note at %d", grooveBar-4+6)
	}
	// pitches preserved
	wantNotes := map[int]int{}
	for _, ev := range in {
		wantNotes[ev.MIDINote]++
	}
	for _, ev := range got {
		wantNotes[ev.MIDINote]--
	}
	for note, n := range wantNotes {
		if n != 0 {
			t.Errorf("pitch %d: count mismatch of %d", note, n)
		}
	}
}

func TestGrooveApply_OverlappingNotesStable(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	// overlapping notes of the same pitch at the same tick
	in := midi.AbsEvents{
		{Start: 0, Duration: groovePPQ, Vel: 80, MIDINote: 60},
		{Start: 0, Duration: groovePPQ / 2, Vel: 80, MIDINote: 60},
		{Start: groovePPQ / 2, Duration: groovePPQ, Vel: 80, MIDINote: 60},
	}
	got1, err := tmpl.Apply(in, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	got2, err := tmpl.Apply(in, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Errorf("overlapping notes: non-deterministic result\n%+v\n%+v", got1, got2)
	}
	if len(got1) != len(in) {
		t.Fatalf("expected %d events, got %d", len(in), len(got1))
	}
}

func TestGrooveApply_ErrorsAndClamps(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := quantizedDrums()

	for _, s := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := tmpl.Apply(in, s, true); err == nil {
			t.Errorf("expected an error for non-finite strength %v, got nil", s)
		}
	}
	if _, err := (GrooveTemplate{}).Apply(in, 1.0, true); err != ErrEmptyTemplate {
		t.Errorf("expected ErrEmptyTemplate, got %v", err)
	}
	bad := GrooveTemplate{Cycle: grooveBar, Points: []GroovePoint{{Position: grooveBar, Offset: 1, VelocityRatio: 1}}}
	if _, err := bad.Apply(in, 1.0, true); err == nil {
		t.Error("expected an error for a point outside the cycle, got nil")
	}
	badRatio := GrooveTemplate{Cycle: grooveBar, Points: []GroovePoint{{Position: 0, Offset: 1, VelocityRatio: math.NaN()}}}
	if _, err := badRatio.Apply(in, 1.0, true); err == nil {
		t.Error("expected an error for a non-finite velocity ratio, got nil")
	}

	// out-of-range strengths are clamped, not errors
	if _, err := tmpl.Apply(in, 2.0, true); err != nil {
		t.Errorf("strength above 1 should clamp, got error %v", err)
	}
	if _, err := tmpl.Apply(in, -1.0, true); err != nil {
		t.Errorf("strength below 0 should clamp, got error %v", err)
	}

	// a note matched to a rushing (negative offset) point near tick 0 must
	// not underflow into negative ticks
	rush := GrooveTemplate{Cycle: grooveBar, Points: []GroovePoint{{Position: 0, Offset: -20, VelocityRatio: 1}}}
	got, err := rush.Apply(midi.AbsEvents{{Start: 4, Duration: 10, Vel: 80, MIDINote: 60}}, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if got[0].Start != 0 {
		t.Errorf("expected underflow to clamp to tick 0, got %d", got[0].Start)
	}

	// velocities are clamped into the MIDI range
	loud := GrooveTemplate{Cycle: grooveBar, Points: []GroovePoint{{Position: 0, Offset: 0, VelocityRatio: 4}}}
	got, err = loud.Apply(midi.AbsEvents{{Start: 0, Duration: 10, Vel: 120, MIDINote: 60}}, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if got[0].Vel != 127 {
		t.Errorf("expected velocity to clamp to 127, got %d", got[0].Vel)
	}
	quiet := GrooveTemplate{Cycle: grooveBar, Points: []GroovePoint{{Position: 0, Offset: 0, VelocityRatio: 0}}}
	got, err = quiet.Apply(midi.AbsEvents{{Start: 0, Duration: 10, Vel: 10, MIDINote: 60}}, 1.0, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if got[0].Vel != 1 {
		t.Errorf("expected velocity to clamp to 1, got %d", got[0].Vel)
	}
}

func TestGrooveApply_EncodeReread(t *testing.T) {
	tmpl := mustExtract(t, playedDrums())
	in := append(quantizedDrums(), quantizedChords()...)
	grooved, err := tmpl.Apply(in, 0.75, true)
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}

	// encode the grooved events and read them back
	buff := filebuffer.New(nil)
	enc := midi.NewEncoder(buff, midi.SingleTrack, groovePPQ)
	grooved.ToMIDITrack(enc)
	if err := enc.Write(); err != nil {
		t.Fatalf("failed to encode grooved events: %v", err)
	}
	if _, err := buff.Seek(0, 0); err != nil {
		t.Fatalf("failed to rewind the buffer: %v", err)
	}
	dec := midi.NewDecoder(buff)
	if err := dec.Parse(); err != nil {
		t.Fatalf("failed to re-read grooved events: %v", err)
	}
	reread := dec.Tracks[0].AbsoluteEvents()

	if len(reread) != len(grooved) {
		t.Fatalf("expected %d events after re-read, got %d", len(grooved), len(reread))
	}
	for i, ev := range grooved {
		if !reflect.DeepEqual(ev, reread[i]) {
			t.Errorf("[%d] re-read mismatch\nexpected %+v\ngot      %+v", i, ev, reread[i])
		}
	}
}
