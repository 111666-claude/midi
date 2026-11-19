package midi

import (
	"errors"
	"sort"
)

var (
	// ErrType2NotSupported is returned when trying to build a chord timeline
	// from an asynchronous (Type 2) MIDI file. Type 2 tracks are not aligned
	// on a shared timeline so their notes cannot be merged into chords.
	ErrType2NotSupported = errors.New("chord timeline: MIDI format 2 (asynchronous tracks) is not supported")
	// ErrInvalidTolerance is returned when the attack tolerance is negative.
	ErrInvalidTolerance = errors.New("chord timeline: attack tolerance must be greater than or equal to 0")
)

// DrumChannel is the MIDI channel (0 indexed) reserved for percussion. Notes
// played on this channel are excluded from the chord timeline by default.
const DrumChannel = 9

// ChordQuality describes the quality (type) of a chord.
type ChordQuality int

const (
	// ChordQualityUnknown means the pitch set does not match one of the
	// supported triad qualities.
	ChordQualityUnknown ChordQuality = iota
	// ChordQualityMajor is a major triad (root, major third, perfect fifth).
	ChordQualityMajor
	// ChordQualityMinor is a minor triad (root, minor third, perfect fifth).
	ChordQualityMinor
	// ChordQualityDiminished is a diminished triad (root, minor third, diminished fifth).
	ChordQualityDiminished
	// ChordQualityAugmented is an augmented triad (root, major third, augmented fifth).
	ChordQualityAugmented
	// ChordQualitySuspended2 is a suspended second triad (root, second, perfect fifth).
	ChordQualitySuspended2
	// ChordQualitySuspended4 is a suspended fourth triad (root, fourth, perfect fifth).
	ChordQualitySuspended4
)

// String implements the stringer interface.
func (q ChordQuality) String() string {
	switch q {
	case ChordQualityMajor:
		return "maj"
	case ChordQualityMinor:
		return "min"
	case ChordQualityDiminished:
		return "dim"
	case ChordQualityAugmented:
		return "aug"
	case ChordQualitySuspended2:
		return "sus2"
	case ChordQualitySuspended4:
		return "sus4"
	default:
		return "unknown"
	}
}

// ChordNoteSource records where a single sounding note contributing to a
// chord came from in the source file.
type ChordNoteSource struct {
	// Track is the index of the source track in the decoded file.
	Track int
	// Channel is the MIDI channel (0 indexed) the note was played on.
	Channel int
	// Key is the MIDI note number of the note.
	Key int
	// Start is the absolute tick at which the note starts sounding.
	Start uint64
	// End is the absolute tick at which the note stops sounding, after
	// note off pairing and sustain pedal extension have been applied.
	End uint64
}

// Chord is a single entry of the chord timeline.
type Chord struct {
	// Start is the absolute tick of the first note attack belonging to the
	// chord.
	Start uint64
	// End is the absolute tick at which the chord changes: the earliest end
	// tick of any of the notes making up the chord.
	End uint64
	// Notes is the sorted, deduplicated set of MIDI note numbers sounding
	// together, including the same pitch coming from different tracks.
	Notes []int
	// Root is the MIDI note number of the chord root, that is to say the
	// lowest note whose triad pattern matches the pitch set. When no triad
	// matches, it is the lowest note of the set.
	Root int
	// Quality is the detected chord quality, ChordQualityUnknown if no
	// supported triad matches.
	Quality ChordQuality
	// Name is the human readable chord name, e.g. "Cmaj", "Amin" or "F?"
	// when the quality can't be determined.
	Name string
	// Sources lists every sounding note (one entry per track occurrence)
	// contributing to the chord.
	Sources []ChordNoteSource
	// HasKeySignature reports whether a key signature meta event was active
	// when the chord starts. When false, Key and Scale default to 0 (C major).
	HasKeySignature bool
	// Key is the active key signature: a positive value is the number of
	// sharps, a negative value the number of flats.
	Key int32
	// Scale is the active key signature scale, 0 for major and 1 for minor.
	Scale uint32
}

// ChordTimeline is an ordered collection of chords, sorted by start tick.
type ChordTimeline []*Chord

// ChordExtractor builds a ChordTimeline out of the note events of a decoded
// MIDI file.
type ChordExtractor struct {
	// AttackToleranceTicks is the maximum distance, in ticks, between note
	// attacks for them to be grouped together as one chord. A note joins a
	// group when its start tick is within that many ticks of the earliest
	// attack of the group. 0 only groups notes starting at the exact same
	// tick. A negative value is rejected.
	AttackToleranceTicks int
	// IncludeDrums includes notes played on the drum channel (channel 10,
	// index 9) in the timeline. By default drum notes are ignored.
	IncludeDrums bool
	// FileName is the name of the source MIDI file. It is kept as provenance
	// and exposed through ChordSourceFile.
	FileName string
}

// ChordSourceFile returns the name of the file the chords were extracted
// from.
func (ce *ChordExtractor) ChordSourceFile() string {
	if ce == nil {
		return ""
	}
	return ce.FileName
}

// soundingNote is an internal representation of a note after note on/off
// pairing and sustain pedal extension.
type soundingNote struct {
	channel  int
	key      int
	track    int
	start    uint64
	end      uint64
	velocity int
}

// openNote is a note that has been turned on but not turned off yet. When
// released under a held sustain pedal, releasedAt is set but the note keeps
// sounding until the pedal goes up.
type openNote struct {
	start      uint64
	velocity   int
	releasedAt uint64 // 0 means the key is still held
}

// keySignature is a key signature meta event positioned on the absolute
// timeline.
type keySignature struct {
	at    uint64
	key   int32
	scale uint32
}

// Chords decodes the note events of the passed decoder into cross track
// sounding intervals, groups them into chords using the configured attack
// tolerance and returns the resulting chord timeline.
//
// Note pairing rules (per track, per channel and key):
//   - a note on event with a positive velocity opens a note; overlapping note
//     on events for the same key are queued FIFO,
//   - a note off event, or a note on event with a velocity of 0, closes the
//     oldest open note at the event's tick,
//   - an orphan note off (no matching note on) is ignored,
//   - while the sustain pedal (CC 64, value >= 64) is held, note offs only
//     mark notes as released: they keep sounding until the pedal goes up
//     (value <= 63), at which point they all end on the pedal up tick,
//   - notes still open when a track ends (missing note off) are closed on the
//     last event tick of that track.
func (ce *ChordExtractor) Chords(dec *Decoder) (ChordTimeline, error) {
	if ce == nil {
		return nil, errors.New("chord timeline: nil chord extractor")
	}
	if dec == nil {
		return nil, errors.New("chord timeline: nil decoder")
	}
	if ce.AttackToleranceTicks < 0 {
		return nil, ErrInvalidTolerance
	}
	// Type 2 files contain independent, asynchronous patterns that don't
	// share a common timeline.
	if dec.Format == Asyncronous {
		return nil, ErrType2NotSupported
	}

	notes := []soundingNote{}
	keySigs := []keySignature{}
	for trackID, track := range dec.Tracks {
		trackNotes, trackKeySigs, err := collectTrack(trackID, track, ce.IncludeDrums)
		if err != nil {
			return nil, err
		}
		notes = append(notes, trackNotes...)
		keySigs = append(keySigs, trackKeySigs...)
	}
	sort.Slice(keySigs, func(i, j int) bool { return keySigs[i].at < keySigs[j].at })

	// Deterministic ordering: earliest attack first, then shortest interval,
	// then lowest pitch, then lowest track.
	sort.Slice(notes, func(i, j int) bool {
		a, b := notes[i], notes[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.end != b.end {
			return a.end < b.end
		}
		if a.key != b.key {
			return a.key < b.key
		}
		return a.track < b.track
	})

	timeline := ChordTimeline{}
	for i := 0; i < len(notes); {
		groupStart := notes[i].start
		// groupMaxStart is the latest attack already absorbed by the group,
		// so attacks chain together as long as consecutive attacks stay within
		// the tolerance window.
		groupMaxStart := groupStart
		j := i
		for j < len(notes) && int(notes[j].start-groupMaxStart) <= ce.AttackToleranceTicks {
			groupMaxStart = notes[j].start
			j++
		}
		members := notes[i:j]
		i = j

		chord := buildChord(members)
		applyKeySignature(chord, keySigs)
		timeline = append(timeline, chord)
	}

	return timeline, nil
}

// ChordTimeline is a convenience wrapper extracting a chord timeline using
// default options (drum notes ignored, no source file name).
func (dec *Decoder) ChordTimeline(attackToleranceTicks int) (ChordTimeline, error) {
	ce := &ChordExtractor{AttackToleranceTicks: attackToleranceTicks}
	return ce.Chords(dec)
}

// collectTrack turns the events of a single track into sounding note
// intervals and positioned key signature events.
func collectTrack(trackID int, track *Track, includeDrums bool) ([]soundingNote, []keySignature, error) {
	notes := []soundingNote{}
	keySigs := []keySignature{}
	if track == nil {
		return notes, keySigs, nil
	}

	// open[channel][key] is the FIFO queue of currently open notes.
	open := map[int]map[int][]*openNote{}
	// pedalDown tracks the sustain pedal state per channel.
	pedalDown := map[int]bool{}
	var lastTick uint64

	flush := func(channel int, at uint64) {
		for key, queue := range open[channel] {
			kept := queue[:0]
			for _, n := range queue {
				if n.releasedAt > 0 {
					notes = append(notes, soundingNote{
						channel:  channel,
						key:      key,
						track:    trackID,
						start:    n.start,
						end:      at,
						velocity: n.velocity,
					})
					continue
				}
				kept = append(kept, n)
			}
			if len(kept) == 0 {
				delete(open[channel], key)
			} else {
				open[channel][key] = kept
			}
		}
	}

	for _, ev := range track.Events {
		if ev.AbsTicks > lastTick {
			lastTick = ev.AbsTicks
		}
		channel := int(ev.MsgChan)

		// Key signature meta events are collected regardless of the channel.
		if ev.MsgType == EventByteMap["Meta"] && ev.Cmd == MetaByteMap["Key Signature"] {
			keySigs = append(keySigs, keySignature{at: ev.AbsTicks, key: ev.Key, scale: ev.Scale})
			continue
		}

		isNoteOn := ev.MsgType == EventByteMap["NoteOn"]
		isNoteOff := ev.MsgType == EventByteMap["NoteOff"]
		if !isNoteOn && !isNoteOff {
			// Sustain pedal handling.
			if ev.MsgType == EventByteMap["ControlChange"] && ev.Controller == 64 {
				if ev.NewValue >= 64 {
					pedalDown[channel] = true
				} else {
					pedalDown[channel] = false
					flush(channel, ev.AbsTicks)
				}
			}
			continue
		}

		if channel == DrumChannel && !includeDrums {
			continue
		}

		key := int(ev.Note)
		// A note on with a velocity of 0 is equivalent to a note off.
		off := isNoteOff || (isNoteOn && ev.Velocity == 0)
		if off {
			if open[channel] == nil || len(open[channel][key]) == 0 {
				// Orphan note off, nothing to pair.
				continue
			}
			queue := open[channel][key]
			if pedalDown[channel] {
				// The note keeps sounding until the pedal goes up. Pair the
				// note off FIFO with the oldest note still held, so repeated
				// note offs under a held pedal mark successive notes.
				for _, n := range queue {
					if n.releasedAt == 0 {
						n.releasedAt = ev.AbsTicks
						break
					}
				}
				continue
			}
			n := queue[0]
			open[channel][key] = queue[1:]
			if len(open[channel][key]) == 0 {
				delete(open[channel], key)
			}
			notes = append(notes, soundingNote{
				channel:  channel,
				key:      key,
				track:    trackID,
				start:    n.start,
				end:      ev.AbsTicks,
				velocity: n.velocity,
			})
			continue
		}

		// Positive velocity note on: open a new note, queued FIFO so
		// overlapping note on events pair deterministically.
		if open[channel] == nil {
			open[channel] = map[int][]*openNote{}
		}
		open[channel][key] = append(open[channel][key], &openNote{
			start:      ev.AbsTicks,
			velocity:   int(ev.Velocity),
			releasedAt: 0,
		})
	}

	// Notes still open at the end of the track (missing note off, or a pedal
	// held until the end) are closed on the last event tick.
	for channel, keys := range open {
		for key, queue := range keys {
			for _, n := range queue {
				notes = append(notes, soundingNote{
					channel:  channel,
					key:      key,
					track:    trackID,
					start:    n.start,
					end:      lastTick,
					velocity: n.velocity,
				})
			}
		}
	}

	return notes, keySigs, nil
}

// buildChord turns a group of simultaneous sounding notes into a Chord.
func buildChord(members []soundingNote) *Chord {
	chord := &Chord{Start: members[0].start}

	end := members[0].end
	seen := map[int]bool{}
	for _, n := range members {
		if n.end < end {
			end = n.end
		}
		if !seen[n.key] {
			seen[n.key] = true
			chord.Notes = append(chord.Notes, n.key)
		}
		chord.Sources = append(chord.Sources, ChordNoteSource{
			Track:   n.track,
			Channel: n.channel,
			Key:     n.key,
			Start:   n.start,
			End:     n.end,
		})
	}
	chord.End = end
	sort.Ints(chord.Notes)

	chord.Root, chord.Quality, chord.Name = detectChord(chord.Notes)
	return chord
}

// chordPattern is a triad quality expressed as semitone offsets from the
// root. Patterns are tested in this order so that unambiguous triads take
// precedence over suspended interpretations.
var chordPatterns = []struct {
	quality ChordQuality
	suffix  string
	offsets [3]int
}{
	{ChordQualityMajor, "maj", [3]int{0, 4, 7}},
	{ChordQualityMinor, "min", [3]int{0, 3, 7}},
	{ChordQualityDiminished, "dim", [3]int{0, 3, 6}},
	{ChordQualityAugmented, "aug", [3]int{0, 4, 8}},
	{ChordQualitySuspended4, "sus4", [3]int{0, 5, 7}},
	{ChordQualitySuspended2, "sus2", [3]int{0, 2, 7}},
}

// detectChord finds the root and quality of a sorted, deduplicated set of
// MIDI note numbers. Each distinct pitch class is tried as a root, starting
// from the lowest note, and extra notes not belonging to the matched triad
// are allowed. When nothing matches, the lowest note is reported as root
// with ChordQualityUnknown.
func detectChord(notes []int) (root int, quality ChordQuality, name string) {
	pitchClasses := map[int]bool{}
	for _, n := range notes {
		pitchClasses[n%12] = true
	}

	tried := map[int]bool{}
	for _, n := range notes {
		pc := n % 12
		if tried[pc] {
			continue
		}
		tried[pc] = true
		for _, pattern := range chordPatterns {
			matches := true
			for _, offset := range pattern.offsets {
				if !pitchClasses[(pc+offset)%12] {
					matches = false
					break
				}
			}
			if matches {
				return n, pattern.quality, Notes[pc] + pattern.suffix
			}
		}
	}

	root = notes[0]
	return root, ChordQualityUnknown, Notes[root%12] + "?"
}

// applyKeySignature attaches the key signature active at the chord's start
// tick. Without any key signature event, C major (0, major) is assumed and
// HasKeySignature stays false.
func applyKeySignature(chord *Chord, keySigs []keySignature) {
	var active *keySignature
	for i := range keySigs {
		if keySigs[i].at <= chord.Start {
			active = &keySigs[i]
		} else {
			break
		}
	}
	if active == nil {
		return
	}
	chord.HasKeySignature = true
	chord.Key = active.key
	chord.Scale = active.scale
}
