package tgclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

// 可预览的媒体类型。
const (
	PreviewImage = "image"
	PreviewVideo = "video"
	PreviewAudio = "audio"
	PreviewPDF   = "pdf"
	PreviewMD    = "md"
)

const (
	maxVideoPreview = 10 << 20 // 视频超过 10MB 不预览（用户要求）
	maxPreview      = 20 << 20 // 其他类型的上限，防止拉大文件
	chunk           = 1 << 20  // upload.getFile 单次上限 1MB；按 1MB 对齐，偏移和长度天然合规
)

// Media 一条消息的媒体信息，全部来自消息本身，不用下载文件。
type Media struct {
	Label   string // 如 "[图片 1280×960 12K]"，没有媒体为空
	Preview string // 可预览类型，空表示不能预览
	Mime    string
	Name    string
	Size    int64
	Loc     tg.InputFileLocationClass // 下载位置，含文件引用；只在预览时临时用
}

// 浏览器能直接显示的图片格式；svg 能带脚本，不放行。
var imageMimes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// describe 根据媒体生成说明文字和预览信息。
func describe(media tg.MessageMediaClass) Media {
	switch v := media.(type) {
	case nil:
		return Media{}
	case *tg.MessageMediaPhoto:
		if p, ok := v.Photo.(*tg.Photo); ok {
			return describePhoto(p)
		}
	case *tg.MessageMediaDocument:
		if d, ok := v.Document.(*tg.Document); ok {
			return describeDoc(d)
		}
	case *tg.MessageMediaGeo:
		if g, ok := v.Geo.(*tg.GeoPoint); ok {
			return Media{Label: fmt.Sprintf("[位置 %.4f,%.4f]", g.Lat, g.Long)}
		}
	case *tg.MessageMediaGeoLive:
		return Media{Label: "[实时位置]"}
	case *tg.MessageMediaVenue:
		return Media{Label: "[地点 " + v.Title + "]"}
	case *tg.MessageMediaContact:
		return Media{Label: "[联系人 " + strings.TrimSpace(v.FirstName+" "+v.LastName+" "+v.PhoneNumber) + "]"}
	case *tg.MessageMediaPoll:
		return Media{Label: "[投票 " + v.Poll.Question.Text + "]"}
	case *tg.MessageMediaDice:
		return Media{Label: fmt.Sprintf("[骰子 %s %d]", v.Emoticon, v.Value)}
	case *tg.MessageMediaWebPage:
		return Media{} // 链接本身就在正文里
	}
	return Media{Label: "[媒体消息]"}
}

func describePhoto(p *tg.Photo) Media {
	var typ string
	var w, h, size int
	for _, s := range p.Sizes {
		var t string
		var sw, sh, sz int
		switch v := s.(type) {
		case *tg.PhotoSize:
			t, sw, sh, sz = v.Type, v.W, v.H, v.Size
		case *tg.PhotoSizeProgressive:
			if n := len(v.Sizes); n > 0 {
				t, sw, sh, sz = v.Type, v.W, v.H, v.Sizes[n-1]
			}
		default:
			continue
		}
		if sw*sh > w*h {
			typ, w, h, size = t, sw, sh, sz
		}
	}
	if typ == "" {
		return Media{Label: "[图片]"}
	}
	return Media{
		Label: fmt.Sprintf("[图片 %d×%d %s]", w, h, humanSize(int64(size))), Preview: PreviewImage,
		Mime: "image/jpeg", Size: int64(size),
		Loc: &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: typ},
	}
}

func describeDoc(d *tg.Document) Media {
	m := Media{Mime: d.MimeType, Size: d.Size,
		Loc: &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}
	var video *tg.DocumentAttributeVideo
	var audio *tg.DocumentAttributeAudio
	var sticker *tg.DocumentAttributeSticker
	gif := false
	for _, a := range d.Attributes {
		switch v := a.(type) {
		case *tg.DocumentAttributeFilename:
			m.Name = v.FileName
		case *tg.DocumentAttributeVideo:
			video = v
		case *tg.DocumentAttributeAudio:
			audio = v
		case *tg.DocumentAttributeSticker:
			sticker = v
		case *tg.DocumentAttributeAnimated:
			gif = true
		}
	}
	size := humanSize(d.Size)
	name := strings.ToLower(m.Name)
	switch {
	case sticker != nil:
		m.Label = strings.TrimSpace("[贴纸 " + sticker.Alt + "]")
	case video != nil:
		kind := "视频"
		if gif {
			kind = "动图"
		} else if video.RoundMessage {
			kind = "视频消息"
		}
		m.Label = fmt.Sprintf("[%s %s %d×%d %s]", kind, duration(int(video.Duration)), video.W, video.H, size)
		if strings.HasPrefix(d.MimeType, "video/") && d.Size <= maxVideoPreview {
			m.Preview = PreviewVideo
		}
	case audio != nil && audio.Voice:
		m.Label = fmt.Sprintf("[语音 %s %s]", duration(audio.Duration), size)
		m.Preview = PreviewAudio
	case audio != nil:
		m.Label = strings.TrimSpace(fmt.Sprintf("[音乐 %s %s", duration(audio.Duration), strings.Trim(audio.Performer+" - "+audio.Title, " -"))) + " " + size + "]"
	default:
		m.Label = strings.TrimSpace("[文件 "+m.Name) + " " + size + "]"
		switch {
		case d.MimeType == "application/pdf" || strings.HasSuffix(name, ".pdf"):
			m.Preview, m.Mime = PreviewPDF, "application/pdf"
		case d.MimeType == "text/markdown" || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown"):
			m.Preview, m.Mime = PreviewMD, "text/plain; charset=utf-8"
		case imageMimes[d.MimeType]:
			m.Preview = PreviewImage
		}
	}
	if m.Preview != "" && m.Preview != PreviewVideo && d.Size > maxPreview {
		m.Preview = ""
	}
	return m
}

// humanSize 1023B / 12K / 8.4M
func humanSize(n int64) string {
	if n < 1<<10 {
		return fmt.Sprintf("%dB", n)
	}
	if k := (n + 1<<10 - 1) >> 10; k < 1<<10 { // K 向上取整，满 1024K 就进位成 M
		return fmt.Sprintf("%dK", k)
	}
	return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
}

// duration 0:12 / 3:05 / 1:02:03
func duration(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// online 等账号连接就绪后返回 API。
func (m *Manager) online(ctx context.Context, accountID uint) (*tg.Client, error) {
	r := m.get(accountID)
	if r == nil {
		return nil, errors.New("账号不在线")
	}
	select {
	case <-r.ready:
		return r.client.API(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("账号连接中，稍后再试")
	}
}

// Media 重新取一次消息拿最新的媒体信息：文件引用会过期，不能用推送时的。
func (m *Manager) Media(ctx context.Context, accountID uint, kind string, peerID, accessHash int64, msgID int) (Media, error) {
	api, err := m.online(ctx, accountID)
	if err != nil {
		return Media{}, err
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}}
	var res tg.MessagesMessagesClass
	if kind == "channel" {
		accessHash = m.peerHash(ctx, api, accountID, kind, peerID, accessHash)
		res, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: peerID, AccessHash: accessHash}, ID: ids})
	} else {
		res, err = api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return Media{}, err
	}
	if mm, ok := res.AsModified(); ok {
		for _, x := range mm.GetMessages() {
			if msg, ok := x.(*tg.Message); ok && msg.ID == msgID {
				return describe(msg.Media), nil
			}
		}
	}
	return Media{}, errors.New("消息已删除")
}

// Download 把文件的 [from, to] 字节段写到 w。跨机房的文件由 gotd 自动转到对应机房。
func (m *Manager) Download(ctx context.Context, accountID uint, loc tg.InputFileLocationClass, from, to int64, w io.Writer) error {
	api, err := m.online(ctx, accountID)
	if err != nil {
		return err
	}
	return copyRange(ctx, func(ctx context.Context, off int64) ([]byte, error) {
		r, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: loc, Offset: off, Limit: chunk})
		if err != nil {
			return nil, err
		}
		f, ok := r.(*tg.UploadFile)
		if !ok {
			return nil, errors.New("不支持的文件响应")
		}
		return f.Bytes, nil
	}, from, to, w)
}

// copyRange 按 1MB 对齐分块拉取 [from, to]，写出当前块的同时拉下一块；
// 内存里最多过两块，不落盘。fetch 返回不满 1MB 表示到文件尾。
func copyRange(ctx context.Context, fetch func(context.Context, int64) ([]byte, error), from, to int64, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type part struct {
		b   []byte
		err error
	}
	parts := make(chan part, 1)
	go func() {
		defer close(parts)
		for off := from &^ (chunk - 1); off <= to; off += chunk {
			b, err := fetch(ctx, off)
			full := len(b) == chunk
			select {
			case parts <- part{cut(b, from-off, to-off+1), err}:
			case <-ctx.Done():
				return
			}
			if err != nil || !full {
				return
			}
		}
	}()
	for p := range parts {
		if p.err != nil {
			return p.err
		}
		if _, err := w.Write(p.b); err != nil {
			return err
		}
	}
	return nil
}

// cut 取 b[lo:hi]，越界自动收窄。
func cut(b []byte, lo, hi int64) []byte {
	n := int64(len(b))
	lo, hi = max(lo, 0), min(hi, n)
	if lo >= hi {
		return nil
	}
	return b[lo:hi]
}
