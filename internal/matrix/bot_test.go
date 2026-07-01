package matrix

import "testing"

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{`!olx add "iphone 13 pro" 100 200 5407`, []string{"!olx", "add", "iphone 13 pro", "100", "200", "5407"}, false},
		{`!olx add "bike" - - -`, []string{"!olx", "add", "bike", "-", "-", "-"}, false},
		{`!olx list`, []string{"!olx", "list"}, false},
		{`!olx add "empty" ""`, []string{"!olx", "add", "empty", ""}, false},
		{`!olx add "oops`, nil, true},
	}
	for _, c := range cases {
		got, err := tokenize(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: token %d got %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestOptInt(t *testing.T) {
	if p, err := optInt("-"); err != nil || p != nil {
		t.Errorf(`optInt("-") = %v, %v`, p, err)
	}
	if p, err := optInt(""); err != nil || p != nil {
		t.Errorf(`optInt("") = %v, %v`, p, err)
	}
	if p, err := optInt("150"); err != nil || p == nil || *p != 150 {
		t.Errorf(`optInt("150") = %v, %v`, p, err)
	}
	if _, err := optInt("abc"); err == nil {
		t.Error(`optInt("abc") expected error`)
	}
}
