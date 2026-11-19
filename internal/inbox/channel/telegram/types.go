package telegram

// Subset of the Telegram Bot API objects the inbox uses.

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type FileRef struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type Contact struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
}

type Message struct {
	MessageID int64       `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Date      int64       `json:"date"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Document  *FileRef    `json:"document"`
	Audio     *FileRef    `json:"audio"`
	Voice     *FileRef    `json:"voice"`
	Video     *FileRef    `json:"video"`
	VideoNote *FileRef    `json:"video_note"`
	Sticker   *FileRef    `json:"sticker"`
	Location  *Location   `json:"location"`
	Contact   *Contact    `json:"contact"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// Media returns the message's downloadable file, its kind and a suggested name, if any.
func (m *Message) Media() (ref FileRef, kind string, ok bool) {
	switch {
	case len(m.Photo) > 0:
		// Sizes are ordered smallest to largest.
		p := m.Photo[len(m.Photo)-1]
		return FileRef{FileID: p.FileID, FileSize: p.FileSize, MimeType: "image/jpeg", FileName: "photo.jpg"}, "photo", true
	case m.Document != nil:
		return *m.Document, "document", true
	case m.Audio != nil:
		return *m.Audio, "audio", true
	case m.Voice != nil:
		r := *m.Voice
		if r.MimeType == "" {
			r.MimeType = "audio/ogg"
		}
		return r, "voice", true
	case m.Video != nil:
		return *m.Video, "video", true
	case m.VideoNote != nil:
		r := *m.VideoNote
		if r.MimeType == "" {
			r.MimeType = "video/mp4"
		}
		return r, "video", true
	case m.Sticker != nil:
		r := *m.Sticker
		if r.MimeType == "" {
			r.MimeType = "image/webp"
		}
		return r, "sticker", true
	}
	return FileRef{}, "", false
}
