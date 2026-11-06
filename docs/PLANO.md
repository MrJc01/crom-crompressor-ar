# Plano: o crom-ar economizar disco de verdade, para qualquer arquivo

Base: as medições de [DIAGNOSTICO-armazenamento.md](DIAGNOSTICO-armazenamento.md)
(05/11/2026, 400 MB de dados reais + vídeos). Checklist executável em
[CHECKLIST.md](CHECKLIST.md).

## O que o sistema tem que fazer

1. Jogar qualquer coisa no cofre (texto, banco, código, binário, foto, vídeo,
   pastas inteiras) e **o disco usado cair de verdade**, contando tudo
   (`vault/` + `brains/` + `spool/` + índices).
2. **Abrir os arquivos normalmente** pelo FUSE, pelo painel e pela extração, sem
   descomprimir o arquivo inteiro e sem duplicar no disco para ler.
3. **Nunca perder dado**: todo arquivo volta bit a bit (SHA-256), inclusive depois
   de `kill -9`, queda de energia ou CLI e residente mexendo juntos.
4. Escalar: arquivos maiores que a RAM, cofres de centenas de GB, 100 mil arquivos
   pequenos.
5. Continuar no ecossistema CROM: `.cromar` portátil e sincronização entre
   máquinas usando o cérebro do crompressor.

## Metas medidas (critério de pronto)

Medidas com `tests/bench_real.sh` sobre os conjuntos de dados reais. O disco conta
o cofre inteiro.

| Tipo de dado | Hoje | Meta |
|---|---|---|
| Textos diferentes (livros) | 1,32× o zstd -19 | ≤ 1,10× o zstd -19 |
| Bancos e dumps SQL | 1,83× o zstd -19 | ≤ 1,15× o zstd -19 |
| Versões do mesmo dado | 3,7× o tar+zstd --long | ≤ 1,15× o tar+zstd --long |
| Vídeo / já comprimido (mp4, jpg, zip) | ~100% | ≤ 100,5% + cópias idênticas e trechos repetidos custam ~0 |
| Ler 1 MB no meio de um arquivo de 1 GB pelo FUSE | descomprime 1 GB no `spool/` | < 50 ms, sem gravar no disco |
| Importar 1 GB | n/d | ≥ 50 MB/s, RAM < 512 MB |

## A decisão principal (D1): onde ficam os blocos

Duas formas de ter dedup entre arquivos:

**A) Cérebro do crompressor como depósito.** Com o motor compilado do fonte atual
(commit `f88b72f`), o `train` com blocos de 4 KB guarda todos os blocos, e uma
versão nova do Northwind vira um `.crom` de 22 KB. Problemas para um cofre local:
- o cérebro fica no disco sem compressão (24,7 MB para um banco de 24,7 MB);
- precisa caber na RAM, o que não serve para um cofre de 100 GB com vídeos;
- cresce só por `train --update`, que regrava o cérebro e ainda não foi testado
  sem invalidar os `.crom` antigos (pendência do próprio motor);
- blocos fixos de 4 KB não acham conteúdo deslocado entre arquivos sem o
  hash rolante, que exige blocos ≥ 1 KB alinhados com as entradas.

**B) Depósito de blocos do próprio cofre (recomendado).** CDC (blocos de tamanho
variável pelo conteúdo) + dedup global + blocos novos agrupados em segmentos de
~1 MB comprimidos com zstd. O protótipo já mediu: livros 5,6 MB, bancos 11,3 MB,
versões 69,5 MB, vídeos com cópia idêntica 71% do original. Não precisa caber na
RAM (índice em disco), lê por trecho e é o modelo testado de restic/borg.
O crompressor fica onde ele rende: `.cromar` e sincronização (fase 6).

Sem a decisão D1 a fase 2 não começa.

## Fases

### Fase 0 — Motor certo e medição honesta
- Compilar o motor **a partir do fonte** `../crompressor` no `make build`. O binário
  de `../crompressor/bin` estava mais velho que o código, sem a correção do treino.
- `go.mod` com `replace => ../crompressor` (feito) e conferência de versão no
  `doctor` (avisar quando o motor do cofre é antigo).
- `stats` e painel contando `brains/`, `spool/`, índices e manifest no "disco".
- `tests/bench_real.sh`: baixa os conjuntos (Gutenberg, Chinook, Sakila, Northwind,
  versões geradas), importa num cofre temporário, compara com zstd e tar+zstd
  --long e grava uma tabela em `tests/BENCH.md`.

### Fase 1 — Formato do depósito (`internal/store`)
- **Blocos:** FastCDC com mínimo 4 KB, média 16 KB e máximo 64 KB. Hash do bloco:
  BLAKE3 ou SHA-256 de 32 bytes.
- **Segmentos** `store/seg/XXXXXXXX.seg`: append-only, ~1 MB de blocos novos por
  frame zstd, com cabeçalho e checksum por frame.
- **Índice** hash → (segmento, frame, offset, tamanho): arquivo ordenado +
  log de novos, ou bbolt. Não pode exigir tudo na RAM.
- **Receita do arquivo:** lista de (hash, tamanho), comprimida, guardada pelo
  SHA-256 do arquivo inteiro (a dedup de arquivo inteiro continua de graça).
- **Contagem de referências** por bloco e **GC**: segmento com muito espaço morto
  é reescrito (`doctor --prune`, também automático em segundo plano).
- **Durabilidade:** escrita em tmp + fsync + rename, journal de transações; um
  import interrompido não deixa referência para bloco inexistente.
- Versão do formato no cabeçalho, para migrar depois sem quebrar.

### Fase 2 — Importar, extrair, verificar
- `add`: lê em streaming (sem carregar o arquivo inteiro), corta, deduplica,
  comprime e grava a receita; verificação lê de volta pelo depósito e confere o
  SHA-256 antes de confirmar.
- **Por tipo de conteúdo** (detecção por amostra de entropia + assinatura):
  - texto, código, CSV, JSON, logs, SQL: zstd nível alto;
  - bancos SQLite: testar corte alinhado em páginas (4 KB) contra CDC e ficar
    com o melhor;
  - já comprimido (mp4, mkv, webm, jpg, png, zip, gz, 7z, xz): **sem zstd**
    (só gastaria CPU), mas com dedup de bloco: cópias e trechos repetidos
    custam ~0;
  - aleatório/criptografado: armazenado cru.
- `extract`, `verify`, `info` lendo do depósito; `rm` decrementando referências.
- Concorrência: vários imports juntos, CLI + residente (lock de arquivo no
  depósito, merge do manifest que já existe).

### Fase 3 — Leitura sob demanda (FUSE e painel)
- `Read(offset, n)` acha os blocos do trecho pela receita (busca binária nos
  offsets) e descomprime só os frames necessários, com **cache LRU em RAM**
  (ex.: 64 MB).
- Fim do `spool/` para leitura. Escrita continua indo para tmp e entra no
  depósito no RELEASE.
- Vídeo: tocar no player direto da montagem com avanço/retrocesso (leitura
  aleatória rápida), testar com mpv/VLC.
- Executáveis: rodar direto de `~/CromAr/apps/` (o exec bit já existe).
- `mmap` (SQLite aberto direto do cofre em modo somente leitura) testado.

### Fase 4 — Todos os tipos de arquivo e casos de borda
- Arquivo vazio, 1 byte, maior que a RAM (5 GB+), esparso, nomes unicode/emoji,
  caminho longo, symlink, hardlink, permissões, mtime, pastas vazias.
- 100 mil arquivos pequenos (`node_modules`): receita pequena por arquivo, índice
  sem explodir.
- Cofre cheio/disco cheio: erro claro, nada corrompido.
- Arquivo alterado no meio do import: detectar e refazer.

### Fase 5 — Migração do cofre que já existe
- `crom-ar migrate`: lê cada `.crom` antigo (motor V24 ou novo, com o codebook da
  entrada), regrava no depósito, confere SHA-256, só então remove o `.crom`.
- Backup automático antes (`manifest.json` + `vault/` + `brains/`).
- O cofre real hoje: 187 entradas, 5,7 MB em `vault/` + 1,2 MB em `brains/`.

### Fase 6 — Ecossistema CROM (onde o cérebro rende)
- `.cromar`: exportar um pacote com o crompressor (`--embed-brain` ou cérebro
  treinado do próprio pacote, blocos de 4 KB), roundtrip conferido.
- Sincronizar dois cofres: só os blocos que faltam passam (pelo depósito) ou pelo
  `daemon`/`pull` do crompressor; decidir após medir.
- Revisar `brain`, `bench`, `edge`/esqueleto: o que continua fazendo sentido com o
  depósito, o que sai.

### Fase 7 — Painel, bandeja e documentação
- Painel: disco real, economia por tipo de arquivo, quanto cada arquivo divide com
  outros, progresso de import/GC/migração.
- README reescrito (tira as frases do V24: "unpack exige codebook" etc.).

### Fase 8 — Testes e aceite
- Unitários do depósito (corte, índice, GC, falha no meio, concorrência, `-race`).
- `tests/suite.sh` adaptada ao depósito. **Rodar num terminal do sistema**: de
  dentro do app Claude o FUSE não monta.
- `tests/bench_real.sh` batendo as metas da tabela acima.
- Teste de escala: 10 GB variados, 1 arquivo de 5 GB, 100 mil arquivos.
- Teste de durabilidade: `kill -9` em pontos aleatórios do import e do GC, depois
  `doctor` sem erro e todos os SHA-256 batendo.

### Futuro — vídeo de verdade
MP4/H.264 já removeram a repetição: o depósito só ganha em cópias idênticas e
trechos iguais (cortes, vinhetas repetidas, mesmo arquivo renderizado de novo
com os mesmos bytes). Ganhar entre vídeos diferentes exige trabalhar com quadros
decodificados (pendência do motor: "Ideia: vídeo em cima de um projeto
existente"). Medida em 05/11: 5 renders reais do crom-videos, 46,6 MB → zstd
46,4 MB; depósito 33,3 MB, e todo o ganho veio da cópia idêntica.
