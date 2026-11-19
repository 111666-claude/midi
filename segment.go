package midi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
)

// NoteHandling controls what happens to notes that start before a segment and
// are still ringing when the segment starts.
type NoteHandling int

const (
	// TruncateNotes keeps notes that cross the segment start boundary. A
	// NoteOn is synthesized at the first tick of the segment so the note keeps
	// sounding, and its duration is shortened to end inside the segment.
	TruncateNotes NoteHandling = iota

	// DropNotes discards any note that started before the segment start tick.
	// Only notes fully contained in the segment are kept.
	DropNotes
)

// SegmentOptions describes the absolute tick range to extract as well as the
// behavior to apply to notes crossing the start boundary.
//
// The segment is the half open interval [Start, End): an event located exactly
// on Start is included while an event located exactly on End is not. Ticks are
// absolute, measured from the beginning of the source file.
type SegmentOptions struct {
	// Start is the absolute tick at which the segment begins.
	Start int64
	// End is the absolute tick at which the segment ends (exclusive).
	End int64
	// CrossBoundaryNotes decides whether notes ringing across Start are
	// truncated (kept) or dropped. Defaults to TruncateNotes.
	CrossBoundaryNotes NoteHandling
}

var (
	// ErrInvalidBoundaries is returned when the segment boundaries are
	// negative or when End is before Start.
	ErrInvalidBoundaries = errors.New("invalid segment boundaries")
	// ErrEmptySegment is returned when Start and End are equal, leaving no
	// ticks to extract.
	ErrEmptySegment = errors.New("empty segment")
	// ErrTickOutOfRange is returned when a boundary (or a delta time derived
	// from it) cannot be represented as a MIDI variable length quantity. MIDI
	// delta times are limited to 28 bits, i.e. 0 to 0x0FFFFFFF.
	ErrTickOutOfRange = errors.New("tick value out of MIDI range")
	// ErrUnpairedNote is returned when a NoteOff is found without a matching
	// NoteOn, which means the source track can't be reliably segmented.
	ErrUnpairedNote = errors.New("unpaired note off event")
)

// maxMIDITick is the largest value a MIDI delta time (a 28 bit variable
// length quantity) can represent.
const maxMIDITick = int64(0x0FFFFFFF)

// ExtractSegment cuts the [Start, End) tick interval out of a decoded MIDI
// file and returns an Encoder holding a new, self contained file that starts
// at tick zero and can be written with Encoder.Write or Encoder.EncodeTo.
//
// Only Type 0 (single track) and Type 1 (synchronous multi track) files are
// supported. The Decoder is not modified: every event is copied before being
// moved or rewritten.
//
// State needed at the beginning of the segment (current tempo, program
// changes and controller values) is collected while scanning the events
// preceding the segment and is re-emitted at tick zero of the corresponding
// output track. When several controller events target the same channel and
// controller before the segment, the last one wins (last-write-wins), which
// mirrors how a synthesizer would process them.
//
// Notes still ringing at End are ended with a synthesized NoteOff and a held
// sustain pedal (controller 64) is released first so nothing rings past the
// segment.
func ExtractSegment(dec *Decoder, opts SegmentOptions) (*Encoder, error) {
	if dec == nil {
		return nil, errors.New("can't extract a segment from a nil decoder")
	}
	if dec.Format == Asyncronous {
		return nil, fmt.Errorf("%w: MIDI format 2 tracks are asynchronous and can't be segmented", ErrFmtNotSupported)
	}
	if opts.Start < 0 || opts.End < 0 {
		return nil, fmt.Errorf("%w: ticks can't be negative (start=%d, end=%d)", ErrInvalidBoundaries, opts.Start, opts.End)
	}
	if opts.End < opts.Start {
		return nil, fmt.Errorf("%w: end tick %d is before start tick %d", ErrInvalidBoundaries, opts.End, opts.Start)
	}
	if opts.End == opts.Start {
		return nil, fmt.Errorf("%w: start and end are both tick %d", ErrEmptySegment, opts.Start)
	}
	if opts.Start > maxMIDITick || opts.End > maxMIDITick {
		return nil, fmt.Errorf("%w: boundary tick exceeds max %d", ErrTickOutOfRange, maxMIDITick)
	}
	if length := opts.End - opts.Start; length > maxMIDITick {
		return nil, fmt.Errorf("%w: segment length exceeds max %d ticks", ErrTickOutOfRange, maxMIDITick)
	}

	enc := NewEncoder(nil, dec.Format, dec.TicksPerQuarterNote)
	enc.TimeFormat = dec.TimeFormat

	for _, track := range dec.Tracks {
		newTrack, err := newSegmenter(track, opts, dec.TicksPerQuarterNote).build()
		if err != nil {
			return nil, err
		}
		enc.Tracks = append(enc.Tracks, newTrack)
	}
	return enc, nil
}

// WriteSegment is a convenience wrapper around ExtractSegment which encodes
// the resulting MIDI file directly into w.
func WriteSegment(w io.WriteSeeker, dec *Decoder, opts SegmentOptions) error {
	enc, err := ExtractSegment(dec, opts)
	if err != nil {
		return err
	}
	return enc.EncodeTo(w)
}

// EncodeTo writes the encoder content into the passed writer. It allows an
// encoder created by ExtractSegment to be serialized without having been
// constructed with a writer.
func (e *Encoder) EncodeTo(w io.WriteSeeker) error {
	if e == nil {
		return errors.New("can't write a nil encoder")
	}
	if w == nil {
		return errors.New("can't write into a nil writer")
	}
	prev := e.w
	e.w = w
	defer func() { e.w = prev }()
	return e.Write()
}

// noteKey identifies a note within a track (channel + pitch).
type noteKey struct {
	channel uint8
	note    uint8
}

// activeNote remembers where a ringing note started and its original NoteOn
// velocity.
type activeNote struct {
	vel     uint8
	started int64
}

// ccKey identifies a controller value within a track.
type ccKey struct {
	channel    uint8
	controller uint8
}

// outputEvent keeps an event destined for the new track together with its
// absolute tick within the segment and its stable ordering index.
type outputEvent struct {
	tick  int64
	cat   outputCategory
	order int
	event *Event
}

// outputCategory defines what is emitted and enforces a deterministic
// ordering between events sharing a tick: synthesized setup state comes
// first, then events copied from the source track, then synthesized end
// cleanup (sustain release and note offs).
type outputCategory int

const (
	catSetup outputCategory = iota
	catBody
	catEnd
)

// segmenter holds the scan state while extracting one track.
type segmenter struct {
	track *Track
	opts  SegmentOptions
	ppqn  uint16

	// State accumulated from events preceding the segment, replayed at tick
	// zero of the output.
	tempoState   *Event
	programState map[uint8]*Event
	ccState      map[ccKey]*Event
	// active tracks notes currently ringing; crossing snapshots the notes that
	// were already ringing at the segment start, keeping their velocity even
	// after they get closed inside the segment.
	active   map[noteKey]*activeNote
	crossing map[noteKey]*activeNote
	body     []outputEvent // events located inside [start, end)
	order    int
}

func newSegmenter(track *Track, opts SegmentOptions, ppqn uint16) *segmenter {
	return &segmenter{
		track:        track,
		opts:         opts,
		ppqn:         ppqn,
		programState: map[uint8]*Event{},
		ccState:      map[ccKey]*Event{},
		active:       map[noteKey]*activeNote{},
		crossing:     map[noteKey]*activeNote{},
	}
}

func (s *segmenter) start() int64 { return s.opts.Start }
func (s *segmenter) end() int64   { return s.opts.End }

// addBody copies an event into the segment body, re-based at segment tick 0.
func (s *segmenter) addBody(tick int64, ev *Event) {
	cp := ev.Copy()
	cp.TimeDelta = 0
	s.body = append(s.body, outputEvent{tick: tick - s.start(), cat: catBody, order: s.order, event: cp})
	s.order++
}

// bodyHasAtStart reports whether any source event re-based onto the first
// segment tick matches pred. Such events supersede carried-over snapshots.
func (s *segmenter) bodyHasAtStart(pred func(*Event) bool) bool {
	for _, oe := range s.body {
		if oe.tick == 0 && pred(oe.event) {
			return true
		}
	}
	return false
}

func (s *segmenter) build() (*Track, error) {
	if err := s.scan(); err != nil {
		return nil, err
	}
	return s.assemble()
}

// scan walks the source events once, maintaining the state required at the
// segment start and collecting in-segment events.
func (s *segmenter) scan() error {
	start, end := s.start(), s.end()

	for _, ev := range s.track.Events {
		tick := int64(ev.AbsTicks)

		// Meta events are handled separately: most of them can't be
		// re-encoded by this package and only make sense at track start.
		if ev.MsgType == EventByteMap["Meta"] {
			switch ev.Cmd {
			case MetaByteMap["End of Track"]:
				// The encoder appends a fresh end of track marker.
				continue
			case MetaByteMap["Tempo"]:
				if tick < start {
					s.tempoState = ev.Copy()
				} else if tick < end {
					s.addBody(tick, ev)
				}
			}
			continue
		}

		switch ev.MsgType {
		case EventByteMap["NoteOn"]:
			if ev.Velocity == 0 {
				// A zero velocity NoteOn is the common shorthand for NoteOff.
				if err := s.noteOff(ev, tick); err != nil {
					return err
				}
				continue
			}
			if tick < start {
				key := noteKey{ev.MsgChan, ev.Note}
				n := &activeNote{vel: ev.Velocity, started: tick}
				s.active[key] = n
				s.crossing[key] = n
				continue
			}
			if tick < end {
				key := noteKey{ev.MsgChan, ev.Note}
				// A fresh in-segment NoteOn replaces any earlier crossing
				// note for the same pitch; the crossing one already has its
				// synthesized onset and its NoteOff is in the body.
				delete(s.crossing, key)
				s.active[key] = &activeNote{vel: ev.Velocity, started: tick}
				s.addBody(tick, ev)
			}

		case EventByteMap["NoteOff"]:
			if err := s.noteOff(ev, tick); err != nil {
				return err
			}

		case EventByteMap["ProgramChange"]:
			if tick < start {
				s.programState[ev.MsgChan] = ev.Copy()
			} else if tick < end {
				s.addBody(tick, ev)
			}

		case EventByteMap["ControlChange"]:
			if tick < start {
				// Last write wins for controller conflicts, matching how a
				// synthesizer processes the stream.
				s.ccState[ccKey{ev.MsgChan, ev.Controller}] = ev.Copy()
			} else if tick < end {
				s.addBody(tick, ev)
			}

		default:
			// Pitch bend, aftertouch and any other voice message are only
			// relevant when they happen inside the segment.
			if tick >= start && tick < end {
				s.addBody(tick, ev)
			}
		}
	}
	return nil
}

// noteOff processes a NoteOff (or a zero velocity NoteOn).
func (s *segmenter) noteOff(ev *Event, tick int64) error {
	key := noteKey{ev.MsgChan, ev.Note}
	n, ok := s.active[key]
	if !ok {
		// An off with no matching on is only a real pairing problem when it
		// happens inside the segment. Before the start it pairs with a note we
		// never tracked, and at/after the end it closes a note that began past
		// the segment; in both cases there is nothing to emit.
		if tick >= s.start() && tick < s.end() {
			return fmt.Errorf("%w: channel %d note %d at tick %d", ErrUnpairedNote, ev.MsgChan, ev.Note, tick)
		}
		return nil
	}
	// A note closing exactly on the segment end is properly paired: drop it
	// from the active set so the assembly step doesn't synthesize another
	// NoteOff for it. Only notes ringing past the end stay open.
	if tick >= s.end() {
		if tick == s.end() {
			delete(s.active, key)
		}
		return nil
	}
	delete(s.active, key)

	if n.started < s.start() {
		if tick == s.start() {
			// The note ends exactly on the segment start: it has zero
			// duration inside the segment, so drop it completely rather than
			// emitting a zero length on/off pair.
			delete(s.crossing, key)
			return nil
		}
		// Note started before the segment.
		if s.opts.CrossBoundaryNotes == TruncateNotes {
			s.addBody(tick, ev)
		}
		return nil
	}
	// Note started inside the segment, its NoteOn is already in the body.
	s.addBody(tick, ev)
	return nil
}

// assemble turns the scan state into an output track starting at tick zero.
func (s *segmenter) assemble() (*Track, error) {
	var out []outputEvent

	// 1. State snapshot at tick zero, in a fixed deterministic order:
	// tempo, then program changes (by channel), then controllers
	// (by channel then controller number). A snapshot is skipped whenever an
	// event at the very first segment tick already sets the same state, since
	// that in-segment event supersedes the carried-over one.
	if s.tempoState != nil && !s.bodyHasAtStart(func(e *Event) bool {
		return e.MsgType == EventByteMap["Meta"] && e.Cmd == MetaByteMap["Tempo"]
	}) {
		s.tempoState.TimeDelta = 0
		s.tempoState.AbsTicks = 0
		out = append(out, outputEvent{tick: 0, cat: catSetup, order: s.order, event: s.tempoState})
		s.order++
	}
	programChans := make([]int, 0, len(s.programState))
	for ch := range s.programState {
		programChans = append(programChans, int(ch))
	}
	sort.Ints(programChans)
	for _, ch := range programChans {
		channel := uint8(ch)
		if s.bodyHasAtStart(func(e *Event) bool {
			return e.MsgType == EventByteMap["ProgramChange"] && e.MsgChan == channel
		}) {
			continue
		}
		ev := s.programState[channel]
		ev.TimeDelta = 0
		ev.AbsTicks = 0
		out = append(out, outputEvent{tick: 0, cat: catSetup, order: s.order, event: ev})
		s.order++
	}
	ccKeys := make([]ccKey, 0, len(s.ccState))
	for k := range s.ccState {
		ccKeys = append(ccKeys, k)
	}
	sort.Slice(ccKeys, func(i, j int) bool {
		if ccKeys[i].channel != ccKeys[j].channel {
			return ccKeys[i].channel < ccKeys[j].channel
		}
		return ccKeys[i].controller < ccKeys[j].controller
	})
	for _, k := range ccKeys {
		if s.bodyHasAtStart(func(e *Event) bool {
			return e.MsgType == EventByteMap["ControlChange"] &&
				e.MsgChan == k.channel && e.Controller == k.controller
		}) {
			continue
		}
		ev := s.ccState[k]
		ev.TimeDelta = 0
		ev.AbsTicks = 0
		out = append(out, outputEvent{tick: 0, cat: catSetup, order: s.order, event: ev})
		s.order++
	}

	// 2. Synthesized NoteOns for notes crossing the start boundary. The
	// crossing map keeps every note that was ringing at start even if it has
	// already been closed inside the segment, so each one gets an onset.
	var crossingKeys []noteKey
	for k := range s.crossing {
		crossingKeys = append(crossingKeys, k)
	}
	sort.Slice(crossingKeys, func(i, j int) bool {
		if crossingKeys[i].channel != crossingKeys[j].channel {
			return crossingKeys[i].channel < crossingKeys[j].channel
		}
		return crossingKeys[i].note < crossingKeys[j].note
	})
	if s.opts.CrossBoundaryNotes == TruncateNotes {
		for _, k := range crossingKeys {
			n := s.crossing[k]
			on := NoteOn(int(k.channel), int(k.note), int(n.vel))
			on.TimeDelta = 0
			on.AbsTicks = uint64(s.start())
			out = append(out, outputEvent{tick: 0, cat: catSetup, order: s.order, event: on})
			s.order++
		}
	}

	// 3. Everything that happened inside the segment.
	out = append(out, s.body...)

	// 4. Clean ending: release every still held sustain pedal, then turn off
	// the notes that are still ringing at the segment end. Releasing sustain
	// first guarantees nothing keeps ringing when the segment loops.
	endTick := s.end() - s.start()
	sustainHeld := map[uint8]bool{}
	for _, oe := range s.body {
		if oe.event.MsgType == EventByteMap["ControlChange"] && oe.event.Controller == 64 {
			sustainHeld[oe.event.MsgChan] = oe.event.NewValue >= 64
		}
	}
	// Sustain state inherited from before the segment also counts.
	for k, ev := range s.ccState {
		if k.controller == 64 {
			sustainHeld[k.channel] = ev.NewValue >= 64
		}
	}

	var endEvents []*Event
	heldChans := make([]int, 0, len(sustainHeld))
	for ch, held := range sustainHeld {
		if held {
			heldChans = append(heldChans, int(ch))
		}
	}
	sort.Ints(heldChans)
	for _, ch := range heldChans {
		endEvents = append(endEvents, ControlChange(ch, 64, 0))
	}

	if s.opts.CrossBoundaryNotes == TruncateNotes {
		for _, k := range crossingKeys {
			if _, ok := s.active[k]; ok {
				endEvents = append(endEvents, NoteOff(int(k.channel), int(k.note)))
			}
		}
	}
	// Notes opened inside the segment and still open at the end.
	var openInside []noteKey
	for k, n := range s.active {
		if n.started >= s.start() {
			openInside = append(openInside, k)
		}
	}
	sort.Slice(openInside, func(i, j int) bool {
		if openInside[i].channel != openInside[j].channel {
			return openInside[i].channel < openInside[j].channel
		}
		return openInside[i].note < openInside[j].note
	})
	for _, k := range openInside {
		endEvents = append(endEvents, NoteOff(int(k.channel), int(k.note)))
	}
	for _, ev := range endEvents {
		ev.TimeDelta = 0
		ev.AbsTicks = uint64(s.end())
		out = append(out, outputEvent{tick: endTick, cat: catEnd, order: s.order, event: ev})
		s.order++
	}

	// Stable sort: events sharing a tick are ordered by category (setup
	// state, source events, end cleanup) and finally by their source order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tick != out[j].tick {
			return out[i].tick < out[j].tick
		}
		if out[i].cat != out[j].cat {
			return out[i].cat < out[j].cat
		}
		return out[i].order < out[j].order
	})

	newTrack := &Track{ticksPerBeat: s.ppqn}
	if name := s.track.Name(); name != "" {
		newTrack.SetName(name)
	}
	var lastTick int64
	for _, oe := range out {
		delta := oe.tick - lastTick
		if delta < 0 || delta > maxMIDITick {
			return nil, fmt.Errorf("%w: computed delta %d", ErrTickOutOfRange, delta)
		}
		oe.event.TimeDelta = uint32(delta)
		oe.event.AbsTicks = uint64(oe.tick)
		newTrack.Events = append(newTrack.Events, oe.event)
		newTrack.Size += uint32(len(EncodeVarint(oe.event.TimeDelta))) + oe.event.Size()
		lastTick = oe.tick
	}
	return newTrack, nil
}

// Encode is a convenience helper returning the standalone file bytes of an
// encoder, typically one returned by ExtractSegment.
func (e *Encoder) Encode() ([]byte, error) {
	if e == nil {
		return nil, errors.New("can't encode a nil encoder")
	}
	buf := bytes.NewBuffer(nil)
	if err := e.EncodeTo(newSliceWriteSeeker(buf)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sliceWriteSeeker adapts a bytes.Buffer to io.WriteSeeker. The encoder only
// ever writes sequentially, so Seek just tracks the position.
type sliceWriteSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func newSliceWriteSeeker(buf *bytes.Buffer) *sliceWriteSeeker {
	return &sliceWriteSeeker{buf: buf}
}

func (s *sliceWriteSeeker) Write(p []byte) (int, error) {
	n, err := s.buf.Write(p)
	s.pos += int64(n)
	return n, err
}

func (s *sliceWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	default:
		s.pos = int64(s.buf.Len()) + offset
	}
	return s.pos, nil
}
