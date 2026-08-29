package fusefs

import "testing"

// Regressão do exec bit: apps importados no cofre precisam aparecer
// executáveis (0755) na montagem para rodar direto de ~/CromAr.
func TestFileModePerm(t *testing.T) {
	cases := map[uint32]uint32{
		0:      0o644, // legado (entradas antigas sem modo)
		0o600:  0o644,
		0o644:  0o644,
		0o755:  0o755,
		0o100:  0o755, // só exec do dono
		0o111:  0o755,
		0o100000 + 0o755: 0o755, // S_IFREG vindo do kernel
	}
	for in, want := range cases {
		if got := fileModePerm(in); got != want {
			t.Errorf("fileModePerm(%o) = %o, want %o", in, got, want)
		}
	}
}
