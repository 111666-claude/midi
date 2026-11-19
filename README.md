# midi

The MIDI package is a high level MIDI library to consume and generate MIDI files.

[![GoDoc](https://godoc.org/github.com/go-audio/midi?status.svg)](https://godoc.org/github.com/go-audio/midi)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/midi)](https://goreportcard.com/report/github.com/go-audio/midi)
[![Coverage Status](https://codecov.io/gh/go-audio/midi/graph/badge.svg)](https://codecov.io/gh/go-audio/midi)
[![Build Status](https://travis-ci.org/go-audio/midi.svg)](https://travis-ci.org/go-audio/midi)

## Chord timeline

A multi-track piano roll can be turned into a timeline of chords. Note events
from every track are first paired into cross-track sounding intervals (note on
velocity 0 is treated as a note off, overlapping note ons pair FIFO, and the
sustain pedal CC 64 extends notes until pedal up), then attacks within a
configurable tolerance are grouped into chords. Each chord reports its tick
range, pitch set, root, quality (major, minor, diminished, augmented, suspended)
and the source track of every contributing note. Drum channel (10) notes are
ignored unless requested, and format 2 files are rejected because their tracks
don't share a timeline.

```go
f, _ := os.Open("song.mid")
defer f.Close()
dec := midi.NewDecoder(f)
if err := dec.Decode(); err != nil {
    log.Fatal(err)
}

extractor := &midi.ChordExtractor{
    AttackToleranceTicks: 10,
    FileName:             "song.mid",
}
chords, err := extractor.Chords(dec)
if err != nil {
    log.Fatal(err)
}
for _, c := range chords {
    fmt.Printf("%s from tick %d to %d\n", c.Name, c.Start, c.End)
}
```

## Alternative

Depending on your needs, you might want to check out the excellent
[gomidi](https://github.com/gomidi/midi) library by
[metakeule](https://github.com/metakeule). His library is lower level but more
complete and offers support for live MIDI messaging which this library isn't
trying to do.
