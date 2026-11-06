# Benchmark com dados reais

Gerado por `tests/bench_real.sh`. "Cofre" é o disco **inteiro** do cofre
(blocos, índice, receitas, manifest, codebooks). Todos os conjuntos voltaram bit
a bit (SHA-256 arquivo por arquivo).

## 06/11/2026 — depósito de blocos (padrão a partir de agora)

| conjunto | arquivos | original | cofre antes (.crom, motor novo) | **cofre agora (blocos)** | zstd -19 por arquivo | tar + zstd -19 --long |
|---|---|---|---|---|---|---|
| livros (Gutenberg) | 30 | 15,82 MB | 6,54 MB | **5,52 MB** | 4,95 MB | 4,42 MB |
| bancos (SQLite + dumps SQL) | 8 | 32,91 MB | 11,67 MB | **11,05 MB** | 9,53 MB | 9,08 MB |
| versões (4× Northwind alterado + 5 tars deste repo) | 9 | 95,49 MB | 34,68 MB | **11,15 MB** | 30,05 MB | 7,53 MB |
| vídeos (4 renders mp4 + 1 cópia idêntica) | 5 | 44,48 MB | — | **31,90 MB** | 44,22 MB | 31,34 MB |

Leitura:

- **Versões:** 3,1× menor que antes e 2,7× menor que zstd por arquivo. Uma versão
  nova do Northwind (23,6 MB) acrescenta ~670 KB. Ainda fica 1,5× acima do
  tar+zstd --long, que também aproveita blocos **parecidos** (página SQLite com
  uma linha alterada). O cofre só aproveita blocos **iguais**.
- **Vídeo:** mp4 não comprime (frames ficam crus, sem gastar CPU); a cópia
  idêntica custou zero. Vídeos diferentes não dividem blocos.
- **Textos e bancos diferentes:** 1,12× e 1,16× o zstd -19 (o zstd em Go,
  nível SpeedBestCompression, é um pouco mais fraco que o zstd -19 em C).

Tempo de import: em CPU, ~50 MB/s nos bancos e ~6 MB/s em textos pequenos
(um frame por arquivo, sem paralelismo entre arquivos). Em disco ocupado, o
`fsync` de cada arquivo domina (até 0,26 s por fsync medido).

Reproduzir: `tests/bench_real.sh` (baixa os dados para `~/.cache/crom-ar-bench`;
vídeos com `VIDEOS=/pasta CONJUNTOS="livros bancos versoes videos"`).

## 06/11/2026 — volume: 7 projetos reais, 4,8 GB (`tests/bench_volume.sh`)

Projetos de `~/Documentos/GitHub` (código, worktrees, node_modules, vídeos e
imagens de animação), todos no mesmo cofre. Comparação: um zip/7z por projeto
(como se guarda no dia a dia) e um tar.zst único de tudo junto.

### Crescimento do cofre a cada projeto

| projeto | original | arquivos | cofre acumulado | cofre +projeto | tempo | RAM pico | zip -9 | 7z -mx9 sólido |
|---|---|---|---|---|---|---|---|---|
| opencorp | 901.3 MB | 64222 | **133.4 MB** | +133.4 MB | 92.0 s | 774 MB | 296.1 MB | 135.8 MB |
| opencorp-claude | 1104.9 MB | 37253 | **353.0 MB** | +219.6 MB | 70.8 s | 876 MB | 395.4 MB | 224.3 MB |
| crom-animate-agent | 940.1 MB | 7954 | **949.7 MB** | +596.7 MB | 29.2 s | 841 MB | 615.0 MB | 534.2 MB |
| crom-animate-studio-scratch | 1388.6 MB | 11647 | **1969.7 MB** | +1020.1 MB | 45.2 s | 1014 MB | 1180.5 MB | 857.0 MB |
| crompressor | 256.7 MB | 512 | **2177.9 MB** | +208.2 MB | 10.3 s | 886 MB | 213.7 MB | 192.1 MB |
| crompressor-stream | 51.1 MB | 19076 | **2232.0 MB** | +54.1 MB | 9.9 s | 1201 MB | 56.0 MB | 41.5 MB |
| crom-animacoes | 150.6 MB | 3080 | **2258.1 MB** | +26.0 MB | 5.5 s | 1199 MB | 60.9 MB | 31.4 MB |
| **total** | **4793.4 MB** | | **2258.1 MB** | | | | 2817.5 MB | 2016.4 MB |

tar + zstd -19 --long=27 de tudo num arquivo só: **2148.9 MB** (1545.3 s).

Razão (original ÷ armazenado): cofre 2.12×, zip 1.70×, 7z por projeto 2.38×, tar.zst único 2.23×.

```
  DISCO REAL:         2.2 GB  (47.1% da origem, economia de 2.5 GB)
    depósito blocos:  2.2 GB
```

### Abrir um arquivo de 1.0 MB de dentro de `crom-animate-studio-scratch`

| | tempo |
|---|---|
| cofre (`crom-ar extract`) | 8.8 s |
| 7z sólido (`7z e` de um arquivo) | 39.0 s |

### Apagar o projeto `crom-animate-studio-scratch` do cofre

| | cofre | tempo | RAM pico |
|---|---|---|---|
| antes | 2258.1 MB | | |
| `crom-ar rm` (GC automático) | 1238.7 MB (−1019.4 MB) | 6.6 s | 617 MB |
| `crom-ar doctor --prune` | 1238.7 MB (−1019.4 MB no total) | 1.1 s | |

Para comparar: `7z d` dos .json de dentro do .7z sólido desse projeto (857.0 → 866.2 MB) levou 163.8 s (o 7z regrava o arquivo).

**Leitura:**
- Cofre 2,12× contra zip 1,70×: 560 MB a menos. Projetos com muita repetição
  (worktrees, node_modules) empatam ou ganham do 7z sólido pela dedup entre
  projetos (opencorp 133 vs 136 MB; crom-animacoes +26 vs 31 MB).
- Perde ~12% para o 7z por projeto, quase tudo nos projetos de mídia/JSON
  grande (LZMA comprime mais que o zstd por frame). Ver CHECKLIST item 47.
- Apagar um projeto de 1,4 GB: 6,6 s e libera 1 GB na hora; no 7z, apagar só
  os .json regrava o arquivo (164 s) e ele até cresce.
- Pendências vistas aqui: RAM de pico cresce com o cofre (índice inteiro em
  memória, 1,2 GB com 2,2 GB de cofre) e o `extract` de 1 MB levou 8,8 s.
- Smoke anterior (2 projetos, 201,8 MB): cofre 104,3 MB (1,94×), zip 1,73×,
  7z por projeto 2,77×; `rm` 43 s → 7,1 s.

## 06/11/2026 — por que não comprime mais (crom-animate-agent, 940 MB, cofre novo)

| | antes | depois (amostra em 4 pontos) |
|---|---|---|
| depósito | 624,8 MB | 613,1 MB |
| frames crus | 462 MiB | 425 MiB |
| frames zstd | 415 → 129 MiB | 452 → 155 MiB |
| tempo / RAM do add | 63–86 s / 398 MB | 43 s / 401 MB |

Nos frames comprimíveis (435 MB juntos): zstd-11 127 MB, zstd-19 114 MB, zstd-22 --long 109 MB, xz -9 102 MB.
Cortar em frames de 1 MiB custa ~1% (127 MB inteiro contra 129 MiB em frames). A diferença para o 7z vem do
LZMA mais forte nessa parte; o resto (git packs, mp4, jpg, png) já vem comprimido e nenhum compressor ganha
mais que ~4% nele. O limite em disco é: dedup + o melhor codec sobre o que é único.

### Agrupar por tipo × objetos do git descomprimidos (mesmo projeto)

Experimento `CROM_EXP_TIPO=<pasta> go test ./internal/store -run TestExpTipo` (blocos únicos, frames de 1 MiB):

| montagem dos frames | depósito |
|---|---|
| ordem de chegada (atual) | 603,7 MB |
| agrupado por tipo de arquivo | 603,1 MB |
| por tipo + dicionário zstd treinado por tipo | 603,1 MB |
| ordem de chegada, objetos do `.git` descomprimidos | 546,8 MB |

O `.git` tinha 3.847 objetos soltos em zlib (377 MB, 40% dos dados únicos), cada um comprimido sozinho:
não deduplicavam com os arquivos do projeto nem comprimiam no frame. Todos são recriados byte a byte pela zlib
do sistema (nível 1), então o depósito agora guarda o conteúdo descomprimido e recomprime na leitura.

Cofre novo com isso: **556,4 MB** (antes 613,1; zip -9 615,0; 7z -mx9 sólido 534,2). `add` 69 s / 403 MB de RAM;
`extract` do projeto inteiro 13 s / 241 MB, 7.954 arquivos idênticos, `git fsck` limpo.

## 06/11/2026 — volume: 9 projetos reais, 10,1 GB (`tests/bench_volume.sh`)

Os 7 projetos do teste de 4,8 GB + crom-tv e crom-openllm. Depósito com objetos do git descomprimidos e manifest em fluxo.

| projeto | original | arquivos | cofre acumulado | cofre +projeto | tempo | RAM pico | zip -9 | 7z -mx9 sólido |
|---|---|---|---|---|---|---|---|---|
| opencorp | 901.3 MB | 64222 | **129.5 MB** | +129.5 MB | 42.3 s | 390 MB | 296.1 MB | 135.8 MB |
| opencorp-claude | 1105.0 MB | 37292 | **343.0 MB** | +213.4 MB | 74.6 s | 390 MB | 395.5 MB | 224.5 MB |
| crom-animate-agent | 940.1 MB | 7954 | **875.6 MB** | +532.6 MB | 52.6 s | 390 MB | 615.0 MB | 534.2 MB |
| crom-animate-studio-scratch | 1388.6 MB | 11647 | **1873.3 MB** | +997.7 MB | 192.2 s | 386 MB | 1180.5 MB | 857.0 MB |
| crompressor | 256.7 MB | 512 | **2078.9 MB** | +205.6 MB | 9.4 s | 389 MB | 213.7 MB | 192.1 MB |
| crompressor-stream | 51.1 MB | 19076 | **2131.9 MB** | +53.1 MB | 17.4 s | 389 MB | 56.0 MB | 41.5 MB |
| crom-animacoes | 150.6 MB | 3080 | **2156.4 MB** | +24.5 MB | 15.7 s | 396 MB | 60.9 MB | 31.4 MB |
| crom-tv | 3734.8 MB | 26134 | **2774.3 MB** | +617.9 MB | 94.5 s | 419 MB | 1089.4 MB | 660.6 MB |
| crom-openllm | 1567.9 MB | 34977 | **3351.6 MB** | +577.3 MB | 166.6 s | 444 MB | 721.5 MB | 340.3 MB |
| **total** | **10096.2 MB** | | **3351.6 MB** | | | | 4628.6 MB | 3017.4 MB |

tar + zstd -19 --long=27 de tudo num arquivo só: **3207.6 MB** (2209.1 s).

Razão (original ÷ armazenado): cofre 3.01×, zip 2.18×, 7z por projeto 3.35×, tar.zst único 3.15×.

### Abrir um arquivo de 1.0 MB de dentro de `crom-animate-studio-scratch`

| | tempo |
|---|---|
| cofre (`crom-ar extract`) | 3.3 s |
| 7z sólido (`7z e` de um arquivo) | 17.3 s |

### Apagar o projeto `crom-animate-studio-scratch` do cofre

| | cofre | tempo | RAM pico |
|---|---|---|---|
| antes | 3351.6 MB | | |
| `crom-ar rm` (GC automático) | 2400.6 MB (−951.1 MB) | 19.7 s | 387 MB |
| `crom-ar doctor --prune` | 2354.7 MB (−997.0 MB no total) | 9.8 s | |

Para comparar: `7z d` dos .json de dentro do .7z sólido desse projeto (857.0 → 866.2 MB) levou 179.5 s (o 7z regrava o arquivo).

Leitura:
- RAM de pico estável em 386–444 MB do primeiro ao nono projeto (no teste de 4,8 GB chegava a 1,2 GB).
- O cofre fica 1,28 GB abaixo do zip e 144 MB acima do tar+zstd -19 único; 334 MB (11%) acima de um 7z por projeto.
- Quase toda a diferença para o 7z está em crom-openllm (+577 contra 340 MB) e crom-animate-studio-scratch (+998 contra 857 MB).
  A causa é repetição distante e parecida (não idêntica): frames de 1 MiB não enxergam. Por tipo, frames zstd-11 de
  1 MiB × zstd-19 --long × xz -9: .map 55/15/18 MB, .js 41/18/18, .sst 224/126/128, .node 87/56/56, .wav 167/64/66, .png 288/250/249.

### Frames maiores? (crom-openllm, 1,57 GB)

| frame | cofre | ler 300 arquivos pequenos (frio) |
|---|---|---|
| 1 MiB (padrão) | 632.4 MB | 1.7 ms cada |
| 4 MiB | 613.8 MB | 2.9 ms cada |
| 16 MiB | 608.5 MB | 11.7 ms cada |

Frames de 16 MiB ganham só 4% e deixam a leitura 7× mais lenta: descartado. A repetição que o zstd --long e o xz
pegam está a centenas de MB de distância, longe demais para qualquer frame.

### Blocos parecidos no mesmo frame? (crom-openllm)

Ordenar os blocos únicos por "super-features" (minhash de janelas de 32 bytes, como Finesse/odess) antes de montar
os frames de 1 MiB: 562,5 → 561,5 MB (−0,2%). Agrupar por tipo: 563,7 MB; por tipo + dicionário: 558,9 MB.
Juntar blocos parecidos não ajuda: descartado.

### Frames que ficavam crus sem precisar (06/11/2026)

A amostra de 4 pontos de 32 KB dizia "não comprime" em frames cuja repetição está a mais de 32 KB de distância
(blocos de .sst, binários). Medido com `CROM_EXP_GAP` (exp_gap_test.go) no crom-openllm: 312 frames crus (330 MB)
que o zstd-11 levaria a 254 MB. Agora, se nenhuma amostra ganha, o nível rápido passa no frame inteiro
(0,7 ms por MiB em dado aleatório, 1,5 GB/s) e decide com limite de 99%.

| crom-openllm | cofre |
|---|---|
| antes | 632.4 MB |
| frame inteiro, limite 97% | 568.7 MB |
| frame inteiro, limite 99% (adotado) | **561.2 MB (−11%)** |

crom-animate-studio-scratch: 1054.7 → 1035.2 MB (−1,8%; ali o peso é png e wav, que só encolhem com repetição a centenas de MB).

10 GB de novo com a correção (`SO_COFRE=1 tests/bench_volume.sh`, mesmos 9 projetos): **3351.6 → 3250.6 MB** (3,11×).
crom-openllm +577.3 → +509.2 MB; studio-scratch +997.7 → +979.2 MB. RAM pico 389–455 MB. Agora 43 MB acima do
tar+zstd -19 --long único (3207.6 MB) e 233 MB acima de um 7z por projeto (3017.4 MB); zip 4628.6 MB.

### Bloco parecido guardado como diferença (item 38) — medição

Os .wav do studio-scratch (121 arquivos, 262,6 MB): vários têm o mesmo tamanho e diferem em ~1.500 bytes espalhados
(341 de 410 blocos de 16 KB têm alguma diferença), então a deduplicação exata não pega. Cada bloco novo cujo
minhash bate com um bloco anterior vira zstd com o anterior como dicionário:

| .wav (únicos 114,8 MB) | disco |
|---|---|
| frames em ordem de chegada | 99.6 MB |
| blocos parecidos juntos (reordenar) | 73.6 MB |
| **delta contra o parecido** | **71.9 MB** (1569 blocos em delta somam 1,1 MB) |
| zstd -19 --long (referência) | 64 MB |

### Delta no depósito de verdade (item 38, 06/11/2026)

Implementado em `internal/store/delta.go`: bloco novo com super-feature (minhash do gear hash, 3 features) igual à
de um bloco já guardado vira `hash da base | zstd(bloco, dicionário = base)` dentro do frame; bit alto do tamanho no
índice marca o delta; a base nunca é delta. As features ficam em `feats.log` (60 bytes por bloco), então valem entre
execuções. Só aceita se o delta for menor que metade do bloco, e decodifica para conferir antes de aceitar. O GC
mantém vivas as bases de todos os deltas do índice e move o delta como está. `Options.NoDelta` desliga.

Encoder com dicionário: criar custa 0,16 ms no nível rápido e 0,8 ms no padrão; nos .wav o tamanho é idêntico
(72,6 MB nos dois), então fica o rápido.

| `CROM_EXP_GAP` | sem delta | com delta |
|---|---|---|
| .wav do studio-scratch (262,6 MB) | 101.0 MB | **72.6 MB (−28%)** |
| crom-openllm (1,64 GB) | 561.2 MB | **515.5 MB (−8%)**, 32.327 blocos em delta |
| crom-animate-studio-scratch | 1035.2 MB | **1011.5 MB (−2%)** |

`crom-ar add crom-openllm` num cofre vazio: 520,9 MB em disco, 89,9 s, RAM pico 389 MB (igual a antes do delta).

10 GB de novo com o delta (`SO_COFRE=1 tests/bench_volume.sh`, mesmos 9 projetos): **3250.6 → 3161.2 MB** (3,19×).
Agora 46 MB **abaixo** do tar+zstd -19 --long único (3207.6 MB) e 144 MB acima de um 7z por projeto (3017.4 MB).
Por projeto: studio-scratch +979.2 → +956.4 MB, crom-openllm +509.2 → +464.1 MB, crompressor +205.6 → +190.9 MB.
RAM pico 393–502 MB (antes 389–455; o mapa de features cresce com o cofre).

GC com delta: a primeira versão lia as bases dos deltas em ordem de mapa e decodificava deltas de arquivo de um
bloco só para saber o tamanho — `crom-ar rm` do studio-scratch levou 152 s. Agora lê em ordem de posição e o arquivo
de um bloco em delta grava a receita completa: rm 39,8 s e prune 34,5 s nessa rodada (o disco estava lento: o add do
opencorp levou 221 s contra 42 s na anterior); um prune repetido no mesmo cofre leva 10,7 s (antes do delta, 9,8 s),
dos quais 3,5 s de CPU são achar as bases. `CROM_AR_CPUPROF=arquivo` grava o perfil de CPU de qualquer comando.
