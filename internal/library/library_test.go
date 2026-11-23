package library

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/testutil"
)

// pattern is P(n) of the kosync-conformance golden vectors: byte i is (i*31 + i>>8) mod 256.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>8)
	}
	return b
}

// sparse is the 1 GiB + 1 KiB vector file: zeros except the 16-digit decimal offset at each sample offset.
type sparse struct{}

const sparseSize = 1<<30 + 1024

func (sparse) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	for i := -1; i <= 10; i++ {
		var o int64
		if i >= 0 {
			o = 1024 << (2 * i)
		}
		label := []byte(padOffset(o))
		for j := range label {
			if pos := o + int64(j) - off; pos >= 0 && pos < int64(len(p)) {
				p[pos] = label[j]
			}
		}
	}
	if off+int64(len(p)) > sparseSize {
		return int(sparseSize - off), io.EOF
	}
	return len(p), nil
}

func padOffset(o int64) string {
	s := []byte("0000000000000000")
	for i := len(s) - 1; o > 0; i-- {
		s[i] = byte('0' + o%10)
		o /= 10
	}
	return string(s)
}

func TestPartialMD5GoldenVectors(t *testing.T) {
	for _, tc := range []struct {
		size int
		want string
	}{
		{500, "21f0df72cce9bc7da8dae2512ee5feed"},
		{1024, "f4cd1641040a17288bb6104d9e66bdb5"},
		{1025, "170a888db01c00d8fc3e5d8d84de5838"},
		{2048, "d0e59a0c7b893c3b8d6a9bddbd64e631"},
		{3000, "d0e59a0c7b893c3b8d6a9bddbd64e631"},
		{200000, "1784b150454ef4cf6780ceb94af0386f"},
		{1050000, "0fca5c751677ecbc2dcfe80ea75ace32"},
	} {
		got, err := PartialMD5(bytes.NewReader(pattern(tc.size)), int64(tc.size))
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "P(%d)", tc.size)
	}
	got, err := PartialMD5(sparse{}, sparseSize)
	require.NoError(t, err)
	assert.Equal(t, "266ff27c24919f4cec96f90f9b3231ca", got, "all twelve samples")
}

func TestNormalizeISBN(t *testing.T) {
	for in, want := range map[string]string{
		"978-7-5366-9293-0": "9787536692930",
		"7536692935":        "9787536692930",
		"0-306-40615-2":     "9780306406157",
		"080442957X":        "9780804429573",
	} {
		got, ok := NormalizeISBN(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "9787536692931", "123", "isbn9787536692930", "1234567890123"} {
		_, ok := NormalizeISBN(bad)
		assert.False(t, ok, bad)
	}
}

func TestFilenameMetadata(t *testing.T) {
	for _, tc := range []struct{ in, title, author string }{
		{"《三体》刘慈欣.txt", "三体", "刘慈欣"},
		{"《三体》作者：刘慈欣.txt", "三体", "刘慈欣"},
		{"《凡人修仙传》[忘语].txt", "凡人修仙传", "忘语"},
		{"《诡秘之主》 - 爱潜水的乌贼.txt", "诡秘之主", "爱潜水的乌贼"},
		{"【书名】1-129 作者：起落.txt", "【书名】1-129 作者：起落", "起落"},
		{"/inbox/plain-name.txt", "plain-name", ""},
	} {
		title, author := FilenameMetadata(tc.in)
		assert.Equal(t, tc.title, title, tc.in)
		assert.Equal(t, tc.author, author, tc.in)
	}
}

func TestParseEPUB(t *testing.T) {
	p := testutil.WriteEPUB(t, t.TempDir(), "b.epub", testutil.EPUB{
		Title: "三体", Author: "刘慈欣", ISBN: "978-7-5366-9293-0", Series: "地球往事", SeriesIndex: "1",
		Language: "zh", Description: "<p>Line one</p><p>Line &amp; two</p>", Subjects: []string{"科幻", "科幻"}, Cover: true,
	})
	m := Parse(context.Background(), p, EPUB, "b.epub")
	assert.Equal(t, "三体", m.Title)
	assert.Equal(t, []string{"刘慈欣"}, m.Authors)
	assert.Equal(t, "9787536692930", m.ISBN)
	assert.Equal(t, "地球往事", m.Series)
	require.NotNil(t, m.SeriesIndex)
	assert.InDelta(t, 1.0, *m.SeriesIndex, 0)
	assert.Equal(t, "Line one\nLine & two", m.Description)
	assert.Equal(t, []string{"科幻"}, m.Tags)
	assert.Equal(t, "png", m.CoverExt)
	assert.NotEmpty(t, m.Cover)
}

func TestParseFallsBackToFilename(t *testing.T) {
	p := testutil.WriteFile(t, t.TempDir(), "x.epub", []byte("not a zip"))
	m := Parse(context.Background(), p, EPUB, "《斗破苍穹》天蚕土豆.epub")
	assert.Equal(t, "斗破苍穹", m.Title)
	assert.Equal(t, []string{"天蚕土豆"}, m.Authors)
}

func TestParseCBZ(t *testing.T) {
	p := testutil.WriteCBZ(t, t.TempDir(), "c.cbz", []string{"p10.png", "p2.png", "p1.png", "__MACOSX/._p1.png"},
		`<ComicInfo><Series>Saga</Series><Number>3</Number><Writer>Brian K. Vaughan</Writer><Year>2014</Year><Month>3</Month></ComicInfo>`)
	m := Parse(context.Background(), p, CBZ, "c.cbz")
	assert.Equal(t, "Saga 3", m.Title)
	assert.Equal(t, "Saga", m.Series)
	assert.Equal(t, []string{"Brian K. Vaughan"}, m.Authors)
	assert.Equal(t, "2014-03", m.Published)
	require.NotNil(t, m.PageCount)
	assert.Equal(t, 3, *m.PageCount)
	assert.Equal(t, "png", m.CoverExt)
}

func TestNaturalOrder(t *testing.T) {
	assert.True(t, naturalLess("page2.jpg", "page10.jpg"))
	assert.True(t, naturalLess("a/001.png", "a/02.png"))
	assert.False(t, naturalLess("b.png", "a.png"))
}

func TestNormalizeShelfAndGroupID(t *testing.T) {
	assert.Equal(t, "小说/仙侠", NormalizeShelf(" 小说// 仙侠 /"))
	assert.Equal(t, md5Hex("小说/仙侠")[:7], GroupID("小说/仙侠"))
}
