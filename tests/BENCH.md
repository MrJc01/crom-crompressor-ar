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
