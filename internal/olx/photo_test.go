package olx

import "testing"

func TestPhotoSized(t *testing.T) {
	p := Photo{Link: "https://cdn/x/image;s={width}x{height}", Width: 1386, Height: 1466}

	// Portrait image capped to 1200 on the longer (height) side.
	url, w, h := p.Sized(1200)
	if h != 1200 {
		t.Errorf("expected height 1200, got %d", h)
	}
	if w != 1386*1200/1466 {
		t.Errorf("unexpected scaled width %d", w)
	}
	want := "https://cdn/x/image;s=" + itoaTest(w) + "x1200"
	if url != want {
		t.Errorf("url = %q, want %q", url, want)
	}

	// Already smaller than cap: unchanged.
	small := Photo{Link: "u;s={width}x{height}", Width: 400, Height: 300}
	if url, w, h := small.Sized(1200); w != 400 || h != 300 || url != "u;s=400x300" {
		t.Errorf("small.Sized = %q %d %d", url, w, h)
	}

	// maxSide 0 means native size.
	if _, w, h := p.Sized(0); w != 1386 || h != 1466 {
		t.Errorf("native Sized = %d %d", w, h)
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
