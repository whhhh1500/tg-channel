package tgclient

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
)

func doc(mime string, size int64, attrs ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
	return &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, MimeType: mime, Size: size, Attributes: attrs}}
}

func TestDescribe(t *testing.T) {
	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 2, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoStrippedSize{Type: "i"},
		&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 9000},
		&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{3000, 8000, 12000}},
	}}}
	cases := []struct {
		name    string
		media   tg.MessageMediaClass
		label   string
		preview string
	}{
		{"无媒体", nil, "", ""},
		{"图片取最大尺寸", photo, "[图片 1280×960 12K]", PreviewImage},
		{"视频", doc("video/mp4", 8_800_000, &tg.DocumentAttributeVideo{Duration: 32.4, W: 1280, H: 720}), "[视频 0:32 1280×720 8.4M]", PreviewVideo},
		{"视频超 10MB 不预览", doc("video/mp4", 11<<20, &tg.DocumentAttributeVideo{Duration: 600, W: 1920, H: 1080}), "[视频 10:00 1920×1080 11.0M]", ""},
		{"视频消息", doc("video/mp4", 500_000, &tg.DocumentAttributeVideo{RoundMessage: true, Duration: 12, W: 384, H: 384}), "[视频消息 0:12 384×384 489K]", PreviewVideo},
		{"动图", doc("video/mp4", 200_000, &tg.DocumentAttributeVideo{Duration: 3, W: 480, H: 270}, &tg.DocumentAttributeAnimated{}), "[动图 0:03 480×270 196K]", PreviewVideo},
		{"语音", doc("audio/ogg", 24_000, &tg.DocumentAttributeAudio{Voice: true, Duration: 12}), "[语音 0:12 24K]", PreviewAudio},
		{"音乐不预览", doc("audio/mpeg", 4_300_000, &tg.DocumentAttributeAudio{Duration: 200, Title: "晴天", Performer: "周杰伦"}, &tg.DocumentAttributeFilename{FileName: "a.mp3"}), "[音乐 3:20 周杰伦 - 晴天 4.1M]", ""},
		{"贴纸", doc("image/webp", 20_000, &tg.DocumentAttributeSticker{Alt: "😀"}, &tg.DocumentAttributeImageSize{W: 512, H: 512}), "[贴纸 😀]", ""},
		{"pdf", doc("application/pdf", 2_200_000, &tg.DocumentAttributeFilename{FileName: "季度报告.pdf"}), "[文件 季度报告.pdf 2.1M]", PreviewPDF},
		{"md 按扩展名", doc("application/octet-stream", 3000, &tg.DocumentAttributeFilename{FileName: "README.md"}), "[文件 README.md 3K]", PreviewMD},
		{"图片当文件发", doc("image/png", 300_000, &tg.DocumentAttributeFilename{FileName: "截图.png"}), "[文件 截图.png 293K]", PreviewImage},
		{"svg 不预览", doc("image/svg+xml", 3000, &tg.DocumentAttributeFilename{FileName: "a.svg"}), "[文件 a.svg 3K]", ""},
		{"超大 pdf 不预览", doc("application/pdf", 30<<20, &tg.DocumentAttributeFilename{FileName: "大.pdf"}), "[文件 大.pdf 30.0M]", ""},
		{"普通文件", doc("application/zip", 1<<20, &tg.DocumentAttributeFilename{FileName: "包.zip"}), "[文件 包.zip 1.0M]", ""},
		{"位置", &tg.MessageMediaGeo{Geo: &tg.GeoPoint{Lat: 31.23041, Long: 121.47372}}, "[位置 31.2304,121.4737]", ""},
		{"投票", &tg.MessageMediaPoll{Poll: tg.Poll{Question: tg.TextWithEntities{Text: "周五聚餐？"}}}, "[投票 周五聚餐？]", ""},
		{"骰子", &tg.MessageMediaDice{Emoticon: "🎲", Value: 5}, "[骰子 🎲 5]", ""},
		{"链接预览不加说明", &tg.MessageMediaWebPage{}, "", ""},
		{"未知类型", &tg.MessageMediaUnsupported{}, "[媒体消息]", ""},
	}
	for _, c := range cases {
		m := describe(c.media)
		if m.Label != c.label || m.Preview != c.preview {
			t.Errorf("%s: got %q %q, want %q %q", c.name, m.Label, m.Preview, c.label, c.preview)
		}
		if m.Preview != "" && m.Loc == nil {
			t.Errorf("%s: 可预览但没有下载位置", c.name)
		}
	}
	if m := describe(photo); m.Loc.(*tg.InputPhotoFileLocation).ThumbSize != "y" {
		t.Errorf("图片应下载最大尺寸: %+v", m.Loc)
	}
}

// 按字节段拉取：对齐到 1MB、首尾裁剪、到文件尾停止、出错立即返回。
func TestCopyRange(t *testing.T) {
	file := make([]byte, 2*chunk+12345)
	for i := range file {
		file[i] = byte(i * 7)
	}
	var offsets []int64
	fetch := func(_ context.Context, off int64) ([]byte, error) {
		offsets = append(offsets, off)
		if off%chunk != 0 {
			return nil, errors.New("偏移没对齐 1MB")
		}
		return cut(file, off, off+chunk), nil
	}
	size := int64(len(file))
	for _, r := range [][2]int64{{0, size - 1}, {100, 200}, {chunk - 10, chunk + 10}, {size - 5, size - 1}, {2 * chunk, size - 1}, {0, 1<<62 - 1}} {
		offsets = nil
		var buf bytes.Buffer
		if err := copyRange(context.Background(), fetch, r[0], r[1], &buf); err != nil {
			t.Fatalf("%v: %v", r, err)
		}
		want := file[r[0]:min(r[1]+1, size)]
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("%v: 内容不对，长度 %d / %d", r, buf.Len(), len(want))
		}
		if wantN := int((min(r[1], size-1))/chunk - r[0]/chunk + 1); len(offsets) != wantN {
			t.Fatalf("%v: 拉了 %d 块 %v，应 %d 块", r, len(offsets), offsets, wantN)
		}
	}

	// 中途出错：返回错误，不再继续拉
	offsets = nil
	boom := errors.New("网络断了")
	err := copyRange(context.Background(), func(_ context.Context, off int64) ([]byte, error) {
		offsets = append(offsets, off)
		if off > 0 {
			return nil, boom
		}
		return make([]byte, chunk), nil
	}, 0, size-1, &bytes.Buffer{})
	if !errors.Is(err, boom) || len(offsets) != 2 {
		t.Fatalf("err = %v offsets = %v", err, offsets)
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1K", 12_000: "12K", 1<<20 - 1: "1.0M", 1<<20 - 1<<10: "1023K", 1 << 20: "1.0M", 8_800_000: "8.4M"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %s, want %s", n, got, want)
		}
	}
	if duration(3723) != "1:02:03" || duration(5) != "0:05" {
		t.Errorf("duration: %s %s", duration(3723), duration(5))
	}
}
