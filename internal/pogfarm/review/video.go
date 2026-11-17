package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Limits of the Bot API.
const (
	// MaxUploadBytes is the largest video a bot may upload. The Bot API
	// documents it as "50 MB"; the decimal reading is used because it is
	// the smaller one, so a file under it is under the limit either way.
	MaxUploadBytes = 50_000_000
	// MaxCaptionLength is the longest caption of a video, in characters
	// after entity parsing. Captions here are plain text, so that is the
	// text itself.
	MaxCaptionLength = 1024
	// MaxMessageLength is the longest text of a message.
	MaxMessageLength = 4096
	// MaxCallbackDataBytes is the longest callback data of a button.
	MaxCallbackDataBytes = 64
)

// uploadTimeout bounds a call that uploads a file: 50 MB over a slow uplink
// takes minutes.
const uploadTimeout = 15 * time.Minute

// InlineKeyboardButton is a button under a message. Pressing it sends the
// bot a callback query carrying CallbackData.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup is the keyboard attached to a message: rows of
// buttons.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// VideoUpload is a video to send from a local file.
type VideoUpload struct {
	// Path is the file to upload. It is streamed, never read into memory.
	Path string
	// Caption is plain text: no parse mode is set, so nothing in it is
	// interpreted as markup. Callers keep it within MaxCaptionLength.
	Caption string
	// Duration is the length in seconds, and Width and Height the size in
	// pixels; zero leaves them out and Telegram works them out.
	Duration int
	Width    int
	Height   int
	// Markup is the keyboard under the video, or nil for none.
	Markup *InlineKeyboardMarkup
}

// SendVideo uploads a video file to a chat. The request is a
// multipart/form-data body whose file part is streamed from disk, so the
// whole video is never held in memory. A file that cannot be opened is an
// error that wraps the *os.PathError, so errors.Is(err, fs.ErrNotExist)
// tells a missing file apart.
func (c *Client) SendVideo(ctx context.Context, chatID int64, v VideoUpload) (*Message, error) {
	const method = "sendVideo"
	f, err := os.Open(v.Path)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: %w", method, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("telegram %s: %w", method, err)
	}
	head, tail, contentType, err := videoEnvelope(chatID, v)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("telegram %s: encode request: %w", method, err)
	}
	body := struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), f, bytes.NewReader(tail)), f}
	length := int64(len(head)) + info.Size() + int64(len(tail))

	var sent Message
	if err := c.do(ctx, uploadTimeout, method, body, length, contentType, &sent); err != nil {
		return nil, err
	}
	return &sent, nil
}

// videoEnvelope builds the multipart body of sendVideo except the file
// bytes: head is everything before them (the plain fields and the header of
// the video part), tail is what follows.
func videoEnvelope(chatID int64, v VideoUpload) (head, tail []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{
		{"chat_id", strconv.FormatInt(chatID, 10)},
		{"supports_streaming", "true"},
	}
	if v.Caption != "" {
		fields = append(fields, [2]string{"caption", v.Caption})
	}
	for _, n := range []struct {
		name string
		v    int
	}{{"duration", v.Duration}, {"width", v.Width}, {"height", v.Height}} {
		if n.v > 0 {
			fields = append(fields, [2]string{n.name, strconv.Itoa(n.v)})
		}
	}
	if v.Markup != nil {
		markup, err := json.Marshal(v.Markup)
		if err != nil {
			return nil, nil, "", err
		}
		fields = append(fields, [2]string{"reply_markup", string(markup)})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, nil, "", err
		}
	}
	// The file name is only a hint to Telegram; it is reduced to safe
	// characters so it cannot break the part header.
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r == 0x7f {
			return '_'
		}
		return r
	}, filepath.Base(v.Path))
	_, err = w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="video"; filename="%s"`, name)},
		"Content-Type":        {"video/mp4"},
	})
	if err != nil {
		return nil, nil, "", err
	}
	head = bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := w.Close(); err != nil {
		return nil, nil, "", err
	}
	return head, bytes.Clone(buf.Bytes()), w.FormDataContentType(), nil
}

// SendMessageMarkup sends a plain text message with a keyboard under it.
func (c *Client) SendMessageMarkup(ctx context.Context, chatID int64, text string, markup *InlineKeyboardMarkup) (*Message, error) {
	params := struct {
		ChatID int64                 `json:"chat_id"`
		Text   string                `json:"text"`
		Markup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	}{chatID, text, markup}
	var sent Message
	if err := c.Call(ctx, "sendMessage", params, &sent); err != nil {
		return nil, err
	}
	return &sent, nil
}
