// Package scan finds the VODs of the allowlisted streamers and the moments
// of a VOD that are worth turning into clips.
//
// Everything in it works on the common types of package source, so it knows
// nothing about any particular platform. The detection logic is pure: it does
// no I/O, reads no clock and returns the same output for the same input.
// Discovery (Discoverer) is the part that talks to the sources and the store.
package scan
