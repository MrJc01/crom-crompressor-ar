# 🧬 CROM-AR — cofre deduplicado de arquivos .crom

**Um "WinRAR" para o ecossistema CROM**: você joga qualquer dado para dentro
e o cofre guarda arquivos `.crom` comprimidos numa pasta que cresce conforme
você importa — com deduplicação por conteúdo, reconstrução bit-a-bit no HD
local, ícone na bandeija do sistema e pacotes portáteis auto-contidos.

Motor de compressão: [MrJc01/crompressor](https://github.com/MrJc01/crompressor)
(V24 "Singularity") — CDC (Content-Defined Chunking) + quantização por codebook
+ delta XOR. Este projeto é a camada de cofre/arquivador em cima do motor.

```
┌─────────────────────────────────────────────────────────────┐
│                        crom-ar residente                     │
│   tray 🧬  ──  cofre FUSE (~/CromAr)  ──  painel web (token) │
│      │                    │                       │          │
│   montar/desmontar   drag & drop nativo      busca O(1)      │
│   painel ligável     pack+dedup on-write     relatórios      │
└─────────────────────────────────────────────────────────────┘
          │ export/import
          ▼
   pacote .cromar (zip auto-contido: membros + codebook + meta)
```

## Como funciona

### O cofre
```
~/Documentos/CromAr/            (CROM_AR_ROOT sobrescreve)
├── vault/                      os .crom — um por CONTEÚDO único (nome = sha256)
├── brains/                     codebooks nomeados (vault.cromdb = padrão)
├── spool/                      cache de leitura (descomprimido on-demand, 24h)
├── manifest.json               pastas virtuais + entradas lógicas → .crom
├── config.json                 caminhos e binário do motor
└── fuse.log                    log do modo nativo
```

- **Dedup por SHA-256**: dois arquivos com conteúdo idêntico = **um só `.crom`**,
  duas entradas lógicas. Economia aparece em `stats`.
- **Sempre com codebook**: o V24 exige codebook no unpack e pack sem codebook
  não é self-contained. O cofre treina `brains/vault.cromdb` no primeiro
  import (bootstrap) — com fallback de semente usando binários do sistema.
  Codebooks são portáteis (`brain export|add`) para compartilhar "vocabulários".
- **Verificação lossless**: todo import faz unpack em temp + SHA-256 antes de
  confirmar. Telemetria de **HitRate** é gravada por entrada.

### As três formas de usar
1. **FUSE nativo** (`crom-ar fuse`): `~/CromAr` é uma pasta real. Arrastar
   para dentro comprime (pack + dedup no close), arrastar para fora
   descomprime on-demand, pastas do manifest viram diretórios. Permissões do
   kernel, zero rede.
   - `arquivos/` dentro do mount = **vista de pacotes**: cada pasta é um
     "arquivo .crom coletivo" (estilo WinRAR); criar pasta = criar pacote.
2. **Tray** (`crom-ar tray`): ícone residente perto do relógio — estado do
   cofre, montar/desmontar, painel web ligável, treinar codebook.
   Single-instance. `install --autostart` sobe no login (systemd --user).
3. **Painel web** (`crom-ar gui`): GUI no navegador (127.0.0.1:8619) com
   árvore de pastas, import por upload, extração com verificação, busca
   O(1), relatórios. **Token de 256 bits** (`0600`) + verificação Host/Origin.

### Pacote portátil `.cromar`
```bash
crom-ar export PacoteA -o PacoteA.cromar        # zip: membros + codebook + meta
crom-ar cd list   PacoteA.cromar                # conteúdo sem abrir
crom-ar cd mount  PacoteA.cromar -m /mnt/x      # pasta READ-ONLY (modo CD)
crom-ar cd import PacoteA.cromar                # recria grupo + instala o codebook
crom-ar extract-file PacoteA.cromar             # extrai direto no HD (Nemo)
```
O `.cromar` é **auto-contido** (o codebook viaja dentro) — resolvido o
problema do V24, em que o unpack exige codebook externo. Roundtrip verificado
bit-a-bit no export.

### Esqueleto lossy (ler sem o delta que monta)
```bash
crom-ar export PacoteA -o preview.cromar --esqueleto   # SEM deltas
crom-ar edge pack arquivo.txt -o a.crom --brain x.cromdb
crom-ar edge unpack a.crom -o a.txt --brain x.cromdb
```
Reconstrução: `original = codebook[q] ⊕ Δ`. Sem Δ, o leitor vê
`codebook[q]` — **fidelidade ≈ HitRate**. Com HitRate 100% num domínio
treinado, o esqueleto ficou **94% menor**. Busca `grep` O(1) só funciona em
`.crom` com chunks semânticos (codebook hits).

## Instalação

```bash
git clone https://github.com/MrJc01/crom-crompressor-ar
cd crom-crompressor-ar
make build      # puxa o binário do motor (SRC_HD=/caminho para sobrescrever)
make install    # ~/.local/bin + ícone na área de trabalho + ações
# opcional: make install -- --autostart   (residente no login, systemd --user)
```

O `go.mod` usa um `replace` apontando para o fonte do motor (modo esqueleto
via cromlib). Ajuste `CROM_SRC`/`replace` se o motor estiver em outro lugar.

## Uso rápido (CLI)

```bash
crom-ar tray                    # ícone residente (recomendado)
crom-ar fuse                    # monta ~/CromAr como pasta nativa
crom-ar add ~/pasta             # importa (dedup automático)
crom-ar list | stats            # conteúdo e números
crom-ar extract <nome> -o dir   # recompila no HD com verificação
crom-ar verify <nome>           # reconfere SHA-256
crom-ar info <nome>             # entropia, chunks, HitRate
crom-ar bench <arquivo|pasta>   # testa 6 estratégias e recomenda a melhor
crom-ar brain list|add|use|export|train   # codebooks compartilháveis
```

## Achados de engenharia (por que cada decisão existe)

| Descoberta | Consequência no design |
|---|---|
| V24: unpack **exige** `--codebook` | cofre sempre empacota com brain; pacote portátil **embuti** o codebook |
| Pack sem codebook não é self-contained (falha de integridade em alguns arquivos) | proibido por padrão (`--no-brain` = expert) |
| Trainer falha com dados mínimos | bootstrap semeia com binários do sistema; lossless é garantido pelo Δ XOR |
| bash faz open→dup2→close: **cada close dispara FLUSH** e o kernel pode entregar writes DEPOIS do FLUSH | import no **RELEASE** + janitor + import preguiçoso em GetAttr/OpenDir |
| CREATE com attr zerado → kernel devolve EIO | registro de escritas "em voo" (`pending`) |
| RELEASE é o único ponto "não vêm mais dados" | verificação lossless e dedup rodam lá, com sidecar `.meta` para crash recovery (kill -9 não perde o que já fechou) |
| Subprocesso de pack custa ~1s | semáforo global (3) para imports em massa; bulk 200 arquivos ≈ 3-5s |
| Dados aleatórios/já comprimidos ficam ~100% (+112 B de header) | bypass nativo; HitRate/telemetria expostos para decidir o que vale esqueleto |

## Estrutura do repositório

```
cmd/crom-ar/            CLI (fuse, tray, gui, add, extract, export, cd, brain, bench…)
internal/vault/         cofre: manifest, dedup, pastas virtuais, pacotes, codebooks
internal/fusefs/        filesystem FUSE (pathfs): leitura on-demand, escrita pack+dedup
internal/server/        painel web local (API + UI embutida, protegida por token)
internal/tray/          ícone de bandeija (SNI/D-Bus, ícone PNG gerado em código)
internal/bench/         testes de estratégias de compressão
internal/bundle/        pacote portátil .cromar (export/mount/import/extract)
tests/                  suíte de integração FUSE (17 testes)
bin/                    motor crompressor puxado do HD (não versionado)
```

## Limitações e roadmap

- Cascade L1/L2/L3 (o motor aceita 3 codebooks por pack; o cofre usa 1 por
  pacote hoje) · criptografia por pacote com UX · delta-sync incremental
  (o formato v5+ já tem Merkle) · multi-volume — estão no radar.
- `grep` O(1) requer chunks semânticos; em arquivos 100% literais não há busca.
- Testado em Cinnamon/Nemo (Linux, fuse3). O motor alvo é o V24 local.

## Licença

MIT
