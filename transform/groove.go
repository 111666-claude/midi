package transform

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/go-audio/midi"
)

// Groove extraction and application.
//
// A groove template captures the human feel of a reference performance: how far
// ahead (negative offset) or behind (positive offset) each grid position is
// played, and how much louder or softer it is played. The template cycles
// every Cycle ticks (one bar by default) and can be applied to other events
// with a strength between 0.0 (no change) and 1.0 (full template).

// GroovePoint is a single entry of a groove template.
type GroovePoint struct {
	// Position is the snapped grid position inside the cycle, in ticks.
	Position int
	// Offset is the signed timing shift in ticks observed at this position.
	// Negative values mean the reference plays ahead of the grid (rushing),
	// positive values mean it plays behind the grid (laid back).
	Offset int
	// VelocityRatio is the velocity multiplier at this position relative to
	// the average velocity of the reference. 1.0 means neutral, values above
	// 1.0 mark accents.
	VelocityRatio float64
}

// GrooveTemplate is a cyclic groove pattern extracted from a reference
// performance or built by hand.
type GrooveTemplate struct {
	// Points are the groove entries, sorted by Position.
	Points []GroovePoint
	// Cycle is the length of the repeating pattern in ticks (one bar by
	// default).
	Cycle int
}

var (
	// ErrEmptyReference is returned when extracting a groove from a reference
	// without any events.
	ErrEmptyReference = errors.New("transform: cannot extract a groove from an empty reference")
	// ErrInvalidPPQ is returned when a zero PPQ (ticks per quarter note) is
	// passed.
	ErrInvalidPPQ = errors.New("transform: ppq must be greater than zero")
	// ErrEmptyTemplate is returned when applying a template without points or
	// with a non-positive cycle.
	ErrEmptyTemplate = errors.New("transform: groove template is empty or has an invalid cycle")
)

// ExtractGroove builds a cyclic groove template from reference absolute
// events. Onsets are snapped to a 1/16 grid derived from ppq; the signed
// distance from each onset to its grid point becomes the timing offset and
// the velocity of each onset relative to the average reference velocity
// becomes the velocity ratio. Multiple onsets snapping to the same grid
// point (chords, unison hits) are averaged.
//
// barTicks sets the length of the repeating cycle; pass a value <= 0 to use
// one 4/4 bar (4 * ppq).
func ExtractGroove(ref midi.AbsEvents, ppq uint16, barTicks int) (GrooveTemplate, error) {
	if ppq == 0 {
		return GrooveTemplate{}, ErrInvalidPPQ
	}
	if len(ref) == 0 {
		return GrooveTemplate{}, ErrEmptyReference
	}
	cycle := barTicks
	if cycle <= 0 {
		cycle = 4 * int(ppq)
	}
	// 1/16 grid
	step := int(ppq) / 4
	if step < 1 {
		step = 1
	}

	// average velocity of the reference, our neutral velocity
	var velSum float64
	for _, ev := range ref {
		if ev == nil {
			continue
		}
		if ev.Start < 0 {
			return GrooveTemplate{}, fmt.Errorf("transform: reference event has a negative start (%d)", ev.Start)
		}
		if ev.Vel < 1 || ev.Vel > 127 {
			return GrooveTemplate{}, fmt.Errorf("transform: reference event velocity %d out of range [1, 127]", ev.Vel)
		}
		velSum += float64(ev.Vel)
	}
	avgVel := velSum / float64(len(ref))
	if avgVel <= 0 {
		return GrooveTemplate{}, fmt.Errorf("transform: reference average velocity is not positive")
	}

	type accum struct {
		offsetSum int
		ratioSum  float64
		count     int
	}
	buckets := map[int]*accum{}
	for _, ev := range ref {
		if ev == nil {
			continue
		}
		pos := ev.Start % cycle
		// snap to the closest grid point, wrapping around the cycle
		snapped := ((pos + step/2) / step) * step
		if snapped >= cycle {
			snapped = 0
		}
		offset := pos - snapped
		if snapped == 0 && pos > cycle/2 {
			// wrapped forward to the start of the next cycle
			offset = pos - cycle
		}
		b, ok := buckets[snapped]
		if !ok {
			b = &accum{}
			buckets[snapped] = b
		}
		b.offsetSum += offset
		b.ratioSum += float64(ev.Vel) / avgVel
		b.count++
	}

	tmpl := GrooveTemplate{Cycle: cycle}
	for pos, b := range buckets {
		tmpl.Points = append(tmpl.Points, GroovePoint{
			Position:      pos,
			Offset:        b.offsetSum / b.count,
			VelocityRatio: b.ratioSum / float64(b.count),
		})
	}
	sort.Slice(tmpl.Points, func(i, j int) bool {
		return tmpl.Points[i].Position < tmpl.Points[j].Position
	})
	return tmpl, nil
}

// Validate checks that the template is well formed and usable by Apply.
func (t GrooveTemplate) Validate() error {
	if t.Cycle <= 0 || len(t.Points) == 0 {
		return ErrEmptyTemplate
	}
	for _, p := range t.Points {
		if p.Position < 0 || p.Position >= t.Cycle {
			return fmt.Errorf("transform: groove point position %d out of cycle [0, %d)", p.Position, t.Cycle)
		}
		if math.IsNaN(p.VelocityRatio) || math.IsInf(p.VelocityRatio, 0) || p.VelocityRatio < 0 {
			return fmt.Errorf("transform: groove point at %d has an invalid velocity ratio %v", p.Position, p.VelocityRatio)
		}
	}
	return nil
}

// nearestPoint returns the template point closest to pos (a position inside
// the cycle), taking the cyclic wrap-around into account.
func (t GrooveTemplate) nearestPoint(pos int) GroovePoint {
	best := t.Points[0]
	bestDist := math.MaxInt32
	for _, p := range t.Points {
		d := pos - p.Position
		if d < 0 {
			d = -d
		}
		if wrap := t.Cycle - d; wrap < d {
			d = wrap
		}
		if d < bestDist {
			bestDist = d
			best = p
		}
	}
	return best
}

// Apply returns a copy of events with the groove template applied at the
// given strength. A strength of 0.0 returns an identical copy, 1.0 applies
// the full template; values outside [0, 1] are clamped and non-finite values
// return an error.
//
// Only note onsets and velocities are affected: pitch is preserved and no
// other event types are touched. When moveNoteOff is true the note off moves
// along with the note on, preserving the original duration; otherwise the
// note end stays in place and the duration adapts (clamped at zero).
//
// Onsets that would be shifted before tick 0 are clamped to tick 0 so the
// result can be safely re-encoded. The output is sorted deterministically by
// start, duration and note, keeping same-tick chords and overlapping notes
// stable.
func (t GrooveTemplate) Apply(events midi.AbsEvents, strength float64, moveNoteOff bool) (midi.AbsEvents, error) {
	if math.IsNaN(strength) || math.IsInf(strength, 0) {
		return nil, fmt.Errorf("transform: groove strength must be finite, got %v", strength)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if strength < 0 {
		strength = 0
	} else if strength > 1 {
		strength = 1
	}
	out := events.Copy()
	if strength == 0 || len(out) == 0 {
		return out, nil
	}

	for i, ev := range out {
		if ev == nil {
			continue
		}
		pos := ev.Start % t.Cycle
		if pos < 0 {
			pos += t.Cycle
		}
		p := t.nearestPoint(pos)

		delta := int(math.Round(float64(p.Offset) * strength))
		newStart := ev.Start + delta
		if newStart < 0 {
			// clamp instead of overflowing into negative ticks
			newStart = 0
		}

		ratio := 1 + (p.VelocityRatio-1)*strength
		newVel := int(math.Round(float64(ev.Vel) * ratio))
		if newVel < 1 {
			newVel = 1
		} else if newVel > 127 {
			newVel = 127
		}

		if !moveNoteOff {
			// keep the note end in place, adjust the duration
			newDur := ev.End() - newStart
			if newDur < 0 {
				newDur = 0
			}
			out[i].Duration = newDur
		}
		out[i].Start = newStart
		out[i].Vel = newVel
	}

	// stable sort so same-tick notes keep their relative order
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].Duration != out[j].Duration {
			return out[i].Duration < out[j].Duration
		}
		return out[i].MIDINote < out[j].MIDINote
	})
	return out, nil
}
