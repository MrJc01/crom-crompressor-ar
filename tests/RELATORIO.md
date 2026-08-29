# 📋 RELATÓRIO — Campanha de testes crom-ar (29/08/2026)

## Plano executado

| Fase | Escopo | Entrega |
|---|---|---|
| 1 — Unitários Go | Regressões dos bugs da sessão 28/08 + novos pontos | `cmd/crom-ar/main_test.go`, `internal/tray/tray_test.go`, `internal/vault/brain_test.go`, `internal/fusefs/fusefs_test.go`, `internal/server/server_test.go` |
| 2 — Integração bash | Modo/exec na montagem, codebooks com refs, tray store compartilhada, coerência do painel, esqueleto read-only, concorrência | Seções S9–S14 em `tests/suite.sh` + flags novas (`tray --web-addr`) |
| 3 — Execução | `go test ./...` · `tests/suite.sh` · `tests/fuse_integration.sh` | **5 pacotes ok · 82 ✔ 0 ✘ · 17 ✔ 0 ✘** |

## Resultados

- `go test ./...` — **ok** em cmd/crom-ar, internal/{fusefs,tray,vault,server}
- `tests/suite.sh` — **PASS=81 FAIL=0** + 1 limitação conhecida documentada (T14a)
- `tests/fuse_integration.sh` — **PASS=17 FAIL=0**

## Descobertas

### Bugs encontrados pelos testes (todos corrigidos nesta campanha)

1. **`Access()` do FUSE mentia sobre permissões** (achado pelo T9c).
   Respondia `OK` incondicionalmente — `access(X_OK)` via `bash [ -x ]` dava
   verdadeiro para QUALQUER arquivo, mesmo `0644`. O kernel confia nessa
   resposta, e apps que sondam execução antes de rodar eram enganados.
   **Correção**: `Access` agora respeita os bits persistidos (R/W do dono;
   X_OK só com exec bit) — `fusefs.go`.

2. **`RemoveBrain` apagava codebook em uso** (T10b).
   Remover um `.cromdb` referenciado por N entradas deixaria essas entradas
   **inextraíveis** (unpack exige o codebook do pack). **Correção**: guard
   `brainRefCount` recusa a remoção com mensagem clara.

3. **`BrainNameFromPath("")` devolvia `"."`** (TestComputeBrainStats).
   Entradas sem codebook (bypass) apareceriam como linha `"."` no painel de
   codebooks em vez de "sem codebook". **Correção**: `""` → `""`.

4. **`crom-ar add` rejeitava flags depois dos caminhos** (TestParseFlex).
   `add dir --brain x` falhava porque `parseFlex` deixava `fs.NArg()==0`
   ao final. Corrigido para usar a lista posicional coletada.

### Regressões da sessão 28/08 — agora blindadas por teste

| Bug original | Teste que pega a regressão |
|---|---|
| Tray montava FUSE com uma **segunda Store** (painel cego ao FUSE; risco de apagar entradas) | **T11c**: escrita na montagem aparece na `/api/tree` do tray NA HORA |
| `WebAddr` default aplicado depois de copiar `opt` → "Abrir painel" abria `http:///?t=` | `TestNormalizeOptionsWebAddr` (unit) |
| Apps ficavam `0644` na montagem (impossível rodar do cofre) | Unit (`TestNewEntryDerivaModo`, `TestFileModePerm`) + **T9a–T9e** (exec direto, chmod, extração com bit) |

### Postura de segurança — confirmada (unit + S8)

Sem token → 403; token errado → 403; `Host: evil.com` → 403 (anti DNS
rebinding); `Origin: evil.com` → 403; página sem token explica como abrir;
`/api/open` recusa caminho inexistente/arquivo (400) sem efeitos colaterais.

### Coerência do painel (T8p)

A soma das linhas de `/api/brains` (economia por codebook) bate exatamente
com `/api/stats` global (origem e armazenado) — o painel de codebooks não
inventa números.

### Esqueleto lossy (S13)

`cd mount` de pacote `--esqueleto` sobe e é **read-only de verdade**
(`touch` falha).

## ⚠ Limitação conhecida documentada (T14a)

**Save do residente sobrescreve mutações de CLI concorrente.** O tray mantém
o manifest em memória; um `crom-ar add` simultâneo grava no disco; o próximo
Save do residente (ex.: criar pasta pela API) reescreve o manifest com a
memória antiga **e a entrada do CLI se perde** (reproduzido e confirmado).
Mitigações possíveis: `flock` + recarga no Save, merge por UID com
tombstones, ou CLI mutando via API do residente. Não corrigido nesta
campanha (mudança de consistência além do escopo dos testes).

## Ferramenta nova de teste

`crom-ar tray --web-addr <porta>` — permite subir instâncias de teste do
tray sem colidir com o residente real (8619); a suíte usa `:8721`.

## Adenda — análise de especialistas (29/08, tarde)

Após a campanha acima, revisão cruzada encontrou e corrigiu:

5. **Corrida de dados introduzida pela Store compartilhada** — os handlers do
   painel (`/api/tree`, `stats`, `brains`, `packages`, `search`, `train`)
   liam `Man.Entries` sem lock enquanto o FUSE (mesma Store) fazia append.
   **Correção**: `dataMu` (RWMutex fino) + `EntriesSnapshot`/`FoldersSnapshot`
   nos leitores — o painel não bloqueia durante packs. Testes de corrida:
   `TestMutacaoConcorrenteComLeitores` (vault) e `TestTreeSobConcorrencia`
   (server); `make test-race` roda tudo sob `-race` (verde).

6. **rsync re-testado**: `ENOSYS` de utimens sumiu; resta falha de `mkstemp`
   (agora `ENOENT`) em arquivos temporários ocultos `.nome.xxxx` — limitação
   documentada; caminho alternativo `tar | tar` funciona.

7. Artefato `internal/vault/.tmp` (resíduo de teste) removido.

## Execução do backlog (29/08, noite)

| Item | Status |
|---|---|
| P0 commit do trabalho | ✔ 4 commits lógicos |
| **P0 T14a** (residente × CLI) | ✔ **corrigido** — `Save` com merge por UID (dirty/deleted); 4 unitários sob `-race`; T14a virou verificação dura na suíte |
| P1 backup do manifest | ✔ `Save` rotaciona `manifest.json.bak` |
| P1 `crom-ar doctor` | ✔ audita config/manifest/UIDs/órfãos/brains/spool; `--fix` restaura do `.bak`; `--prune` apaga órfãos |
| P1 CI com `-race` | ✔ `go test -race ./...` no workflow |
| P1 README | ✔ painel Codebooks, apps do cofre, doctor/token, novos achados |
| P2 `token rotate` | ✔ |
| P2 rsync | ✔ **RESOLVIDO** — causa real: `fchmod` do rsync em arquivo pendente; `Chmod` agora atende pendentes via opts (T17a/b: rsync sem erros + sha ok) |
| P2 janitor de spool testado | ✔ S15 (poda 24h na remontagem + recriação on-demand) |
| P2 bench A/B automatizado | ✔ S16 (recomendação + ratio cdc < 100%) |

Pendências restantes (não bloqueantes): criptografia por padrão, progresso
por arquivo na GUI, log estruturado do residente, kill -9 durante operação
de API (difícil de automatizar de forma estável).

**Estado final das suítes: 90/90 (suite.sh) · 17/17 (fuse_integration.sh) ·
`go test -race ./...` verde · CI com race detector.**
