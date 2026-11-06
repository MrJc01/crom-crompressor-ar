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
