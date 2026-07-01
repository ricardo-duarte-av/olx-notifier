package olx

import "testing"

func TestCleanDescription(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "br tags and blank lines",
			in:   "Vendo iPhone 12 roxo <br />\n<br />\nCom 64GB <br />\n<br />\nDesbloqueado <br />",
			want: "Vendo iPhone 12 roxo\n\nCom 64GB\n\nDesbloqueado",
		},
		{
			name: "single br per line",
			in:   "iPhone 11 como novo<br />Bateria a 82%<br />Sem marcas",
			want: "iPhone 11 como novo\nBateria a 82%\nSem marcas",
		},
		{
			name: "entities and stray tags",
			in:   "Pre&ccedil;o &amp; estado <b>novo</b> &lt;3",
			want: "Preço & estado novo <3",
		},
		{
			name: "plain text unchanged",
			in:   "Sem HTML aqui",
			want: "Sem HTML aqui",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Offer{Description: c.in}.CleanDescription()
			if got != c.want {
				t.Errorf("CleanDescription()\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}
