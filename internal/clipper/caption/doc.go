// Package caption turns the words of a transcript into the subtitle file
// that the render step burns into the clip.
//
// The file is in ASS (Advanced SubStation Alpha) format, which ffmpeg's
// subtitles and ass filters draw with libass. ASS carries the font, the
// colours, the outline and the position, and lets a single word of a line
// be styled differently. The script's resolution (PlayResX 1080, PlayResY
// 1920) is that of the output frame, so every size and margin in the
// configuration is in pixels of that frame.
//
// The package is pure logic. Its only I/O is writing the one file the
// caller names; it runs no tool.
//
// # Phrases
//
// Words are shown a few at a time, in phrases. Words are added to a phrase
// in spoken order until one of these starts a new phrase:
//
//   - the phrase already holds clipper.caption.max_words words;
//   - the next word would take the phrase past clipper.caption.max_chars
//     characters, counting one space between words (a single word longer
//     than that is a phrase of its own);
//   - the pause between the end of the last word and the start of the next
//     is longer than clipper.caption.max_gap.
//
// A phrase is on screen from the start of its first word to the end of its
// last word, cut short when the next phrase starts earlier, so two phrases
// never overlap. Between phrases nothing is shown. A phrase whose display
// time rounds to nothing (words WhisperX could not time all get the same
// instant) is not written and not counted.
//
// # Highlighting
//
// The word being spoken is drawn in clipper.caption.highlight_color and,
// unless clipper.caption.highlight_scale is 100, slightly larger, for its
// own duration. Each phrase is written as a sequence of Dialogue events
// that follow each other without gaps or overlaps: one per word, which
// repeats the whole phrase with that word highlighted by inline override
// tags, and one without a highlight for each pause between two words of
// the phrase. Exactly one event is therefore visible at any moment while a
// phrase is shown. The alternative, karaoke tags (\k) on one event per
// phrase, can only change colour from the secondary to the primary colour
// and cannot scale a word or turn its colour back after it was spoken.
//
// A word's highlight ends at its own end or at the start of the next word,
// whichever is first. Scaling the highlighted word makes the line a little
// wider, so the centred phrase shifts by a few pixels as the highlight
// moves; at the default 110 % this is barely visible.
//
// # Times and colours
//
// ASS times are written H:MM:SS.cc, in hundredths of a second. Every
// boundary is rounded to the nearest hundredth, the same way for the event
// that ends there and the one that starts there, so events stay contiguous.
//
// Colours are configured as "#RRGGBB" and written in ASS's order, blue
// first: "&HAABBGGRR" in the style, where AA is the transparency (00 is
// opaque), and "&HBBGGRR&" in override tags. The drop shadow uses the
// outline colour at transparency 80 (half opacity).
//
// # Text
//
// Inside ASS dialogue text "{" opens a block of override tags and a
// backslash starts an escape such as \N (line break), so a transcript
// word could break the file or restyle it. Every word is therefore
// cleaned before it is written:
//
//   - line breaks, tabs and other white space inside a word become one
//     space, and other control characters are removed;
//   - with clipper.caption.uppercase on, the word is upper-cased (Unicode
//     aware, one character at a time);
//   - "{" and "}" are written as libass's escapes \{ and \}, and a
//     backslash is followed by U+2060 WORD JOINER, an invisible character,
//     so it is drawn as itself and never starts an escape or a tag.
//
// All other text, including any non-Latin script and emoji, is written
// unchanged as UTF-8. Whether it is drawn depends on the font: Montserrat
// covers Latin, Cyrillic and Vietnamese, and libass falls back to another
// installed font for other characters.
//
// # An empty transcript
//
// A transcript with no words gives a complete file with the header, the
// style and an empty event list, which ffmpeg draws as nothing.
//
// # Checking by eye
//
// The tests check the file's text against golden files and, when ffmpeg is
// installed, that ffmpeg renders every golden file without error. To look
// at the result, run from this directory with ffmpeg built with libass and
// the Montserrat ExtraBold font installed (both are in the clipper image):
//
//	ffmpeg -f lavfi -i color=c=0x336699:s=1080x1920:r=25:d=6 \
//	  -vf ass=testdata/normal.ass -frames:v 1 -ss 1.0 /tmp/caption.png
package caption
