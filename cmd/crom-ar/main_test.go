package main

import (
	"flag"
	"testing"
)

func TestParseFlexOrdemDeFlags(t *testing.T) {
	cases := []struct {
		nome  string
		args  []string
		want  []string // posicionais esperados
		flag  string   // valor esperado de -o/--brain (string flags)
	}{
		{"flags antes", []string{"-o", "out", "dir1", "dir2"}, []string{"dir1", "dir2"}, "out"},
		{"flags depois (bug: fs.NArg()==0)", []string{"dir1", "dir2", "-o", "out"}, []string{"dir1", "dir2"}, "out"},
		{"flags intercaladas", []string{"dir1", "-o", "out", "dir2"}, []string{"dir1", "dir2"}, "out"},
		{"sem flags", []string{"dir1"}, []string{"dir1"}, ""},
		{"flag com espaço no valor", []string{"dir1", "--brain", "b"}, []string{"dir1"}, "b"},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ExitOnError)
		out := fs.String("o", "", "")
		brain := fs.String("brain", "", "")
		pos, err := parseFlex(fs, c.args)
		if err != nil {
			t.Errorf("%s: erro inesperado: %v", c.nome, err)
			continue
		}
		if len(pos) != len(c.want) {
			t.Errorf("%s: posicionais = %v, want %v", c.nome, pos, c.want)
			continue
		}
		for i := range pos {
			if pos[i] != c.want[i] {
				t.Errorf("%s: posicional %d = %q, want %q", c.nome, i, pos[i], c.want[i])
			}
		}
		var got string
		switch c.flag {
		case "b":
			got = *brain
		default:
			got = *out
		}
		if got != c.flag {
			t.Errorf("%s: flag = %q, want %q", c.nome, got, c.flag)
		}
	}
}
