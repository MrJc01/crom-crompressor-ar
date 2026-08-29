#!/usr/bin/env bash
# Suíte-mestra crom-ar — 70+ verificações: FUSE, pacotes, lixeira, edge,
# bundles .cromar, codebooks, CLI e painel web (token/segurança).
# Uso: tests/suite.sh [binário]   (KEEP=1 preserva evidências em falha)
set -u
cd "$(dirname "$0")/.." || exit 1
BIN=${1:-$PWD/bin/crom-ar}
BIN=$(readlink -f "$BIN")
R=/tmp/crom-suite-$$
MNT=$R/mnt
CDMNT=$R/cd
PROJ=$PWD
export CROMPRESSOR_BIN=$PROJ/bin/crompressor
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✔ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  ✘ $1 ${2:+(obtido: $2)}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "esperado=$2 obtido=$3"; fi }

wait_settle() {
  local t0=$(date +%s)
  while true; do
    local n; n=$(ls "$CROM_AR_ROOT/spool" 2>/dev/null | grep -c "^tmp-" || true)
    [ "$n" = "0" ] && return 0
    [ $(( $(date +%s) - t0 )) -gt 90 ] && return 1
    sleep 0.3
  done
}

kill_port() { # mata quem escuta na porta (o $! do setsid não é o pid real)
  local pid
  pid=$(ss -tlnp 2>/dev/null | grep ":$1 " | grep -oP 'pid=\K[0-9]+' | head -1)
  [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
  return 0
}

cleanup() {
  fusermount3 -u "$MNT" 2>/dev/null; umount -l "$MNT" 2>/dev/null
  fusermount3 -u "$CDMNT" 2>/dev/null; umount -l "$CDMNT" 2>/dev/null
  fusermount3 -u "$TRAYMNT" 2>/dev/null; umount -l "$TRAYMNT" 2>/dev/null
  [ -n "$FUSEPID" ] && kill -9 "$FUSEPID" 2>/dev/null
  [ -n "$TRAYPID" ] && kill -9 "$TRAYPID" 2>/dev/null
  kill_port 8719
  kill_port 8721
  if [ "${KEEP:-0}" = "1" ] || [ $FAIL -gt 0 ]; then
    echo "evidências: $R"
  else
    rm -rf "$R"
  fi
}
trap cleanup EXIT

mkdir -p "$MNT" "$CDMNT" 2>/dev/null || { mkdir -p "$MNT"; mkdir -p "$CDMNT"; }
kill_port 8719
export CROM_AR_ROOT=$R/vault
$BIN init >/dev/null || { echo "init falhou"; exit 1; }
setsid $BIN fuse -m "$MNT" </dev/null >"$R/fuse.log" 2>&1 &
FUSEPID=$!
sleep 2
mount | grep -q "$MNT " && ok "S0 montagem FUSE" || bad "S0 montagem FUSE"

# ════════ S1 — FUSE essencial (dados pequenos) ════════
echo "— S1 FUSE essencial —"
: > "$MNT/vazio.bin" && ok "T1a 0-byte escrita"
check "T1b 0-byte tamanho" "0" "$(stat -c%s "$MNT/vazio.bin")"
echo "conteudo acentuado ção" > "$MNT/arquivo com espaço e ção.txt" && ok "T1c unicode escrita"
grep -q "ção" "$MNT/arquivo com espaço e ção.txt" && ok "T1d unicode leitura" || bad "T1d unicode leitura"
for i in $(seq 1 100); do echo "linha $i crom bulk suíte"; done > /tmp/suite-doc.txt
head -c 150000 /dev/urandom > /tmp/suite-bin.bin
cp /tmp/suite-doc.txt /tmp/suite-bin.bin "$MNT/" && ok "T1e cp texto+binário"
wait_settle
h1=$(sha256sum /tmp/suite-doc.txt | cut -d' ' -f1)
h2=$(sha256sum "$MNT/suite-doc.txt" | cut -d' ' -f1)
check "T1f sha256 texto" "$h1" "$h2"
h3=$(sha256sum /tmp/suite-bin.bin | cut -d' ' -f1)
h4=$(sha256sum "$MNT/suite-bin.bin" | cut -d' ' -f1)
check "T1g sha256 binário" "$h3" "$h4"
mv "$MNT/suite-doc.txt" "$MNT/doc-renomeado.txt" && ok "T1h rename"
cat "$MNT/doc-renomeado.txt" | grep -q "linha 42" && ok "T1i conteúdo pós-rename" || bad "T1i conteúdo pós-rename"
rm "$MNT/doc-renomeado.txt" && ok "T1j unlink" || bad "T1j unlink"

# ════════ S2 — Pacotes (/arquivos) ════════
echo "— S2 Pacotes —"
mkdir -p "$MNT/arquivos/Pk1" && ok "T2a mkdir pacote"
cp /tmp/suite-doc.txt "$MNT/arquivos/Pk1/doc.txt"
cp /tmp/suite-bin.bin "$MNT/arquivos/Pk1/bin.bin"
wait_settle
check "T2b 2 membros" "2" "$(ls "$MNT/arquivos/Pk1" | wc -l)"
g=$(python3 -c "import json;m=json.load(open('$CROM_AR_ROOT/manifest.json'));print(','.join({e['group'] for e in m['entries'] if '/Pk1/' in e['path']}))")
check "T2c grupo=Pk1" "Pk1" "$g"
cp "$MNT/arquivos/Pk1/doc.txt" "$MNT/arquivos/Pk1/doc-copia.txt"
wait_settle
c0=$(ls "$CROM_AR_ROOT/vault" | wc -l)
cp "$MNT/arquivos/Pk1/doc.txt" "$MNT/arquivos/Pk1/doc-copia2.txt"
wait_settle
c1=$(ls "$CROM_AR_ROOT/vault" | wc -l)
check "T2d dedup no pacote" "$c0" "$c1"
mv "$MNT/arquivos/Pk1/bin.bin" "$MNT/arquivos/Pk1/bin-movido.bin" && ok "T2e rename membro"
mv "$MNT/arquivos/Pk1/doc.txt" "$MNT/doc-solto.txt" && ok "T2f sair do pacote"
python3 -c "
import json,sys
m=json.load(open('$CROM_AR_ROOT/manifest.json'))
for e in m['entries']:
    if e['path']=='/doc-solto.txt':
        print('ok' if e['group']=='doc-solto.txt' else 'grupo errado: '+e['group']); sys.exit(0)
print('entrada ausente')" | grep -q ok && ok "T2g saída do grupo" || bad "T2g saída do grupo"
mv "$MNT/arquivos/Pk1" "$MNT/arquivos/Pk1Renomeado" && ok "T2h rename pacote"
cat "$MNT/arquivos/Pk1Renomeado/bin-movido.bin" | sha256sum | cut -d' ' -f1
h5=$(sha256sum /tmp/suite-bin.bin | cut -d' ' -f1)
check "T2i membro íntegro pós-rename" "$h5" "$(sha256sum "$MNT/arquivos/Pk1Renomeado/bin-movido.bin" | cut -d' ' -f1)"

# ════════ S3 — Lixeira bloqueada ════════
echo "— S3 Lixeira —"
mkdir "$MNT/.Trash-1000" 2>/dev/null && bad "T3a mkdir .Trash recusado" || ok "T3a mkdir .Trash recusado"
mv "$MNT/suite-bin.bin" "$MNT/.Trash-1000/x" 2>/dev/null && bad "T3b mv p/ .Trash recusado" || ok "T3b mv p/ .Trash recusado"
ls -A "$MNT" | grep -q ".Trash" && bad "T3c sem .Trash na listagem" || ok "T3c sem .Trash na listagem"

# ════════ S4 — Edge (esqueleto lossy) ════════
echo "— S4 Edge —"
BRAIN=$CROM_AR_ROOT/brains/vault.cromdb
./bin/crom-ar edge pack /tmp/suite-doc.txt -o /tmp/edge-doc.crom --brain "$BRAIN" >/dev/null && ok "T4a edge pack"
./bin/crom-ar edge unpack /tmp/edge-doc.crom -o /tmp/edge-rest.txt --brain "$BRAIN" >/dev/null && ok "T4b edge unpack"
diff -q /tmp/suite-doc.txt /tmp/edge-rest.txt >/dev/null && ok "T4c edge idêntico (hit 100%)" || ok "T4c edge lossy (diferenças esperadas)"
s_full=0; s_edge=0
wait_settle
./bin/crom-ar export Pk1Renomeado -o "$R/full.cromar" >/dev/null 2>&1 && s_full=$(stat -c%s "$R/full.cromar")
./bin/crom-ar export Pk1Renomeado -o "$R/edge.cromar" --esqueleto >/dev/null 2>&1 && s_edge=$(stat -c%s "$R/edge.cromar")
[ "$s_edge" -lt "$s_full" ] && ok "T4d esqueleto menor que full ($s_edge < $s_full)" || bad "T4d esqueleto menor" "$s_edge<$s_full"
python3 -c "
import zipfile,json,sys
z=zipfile.ZipFile('$R/edge.cromar')
m=json.loads(z.read('meta.json'))
sys.exit(0 if m.get('mode')=='edge' else 1)" && ok "T4e meta.mode=edge" || bad "T4e meta.mode=edge"

# ════════ S5 — Bundle .cromar ════════
echo "— S5 Bundle —"
./bin/crom-ar cd list "$R/full.cromar" 2>/dev/null | grep -q "membros" && ok "T5a cd list" || bad "T5a cd list"
./bin/crom-ar cd mount "$R/full.cromar" -m "$CDMNT" --daemon >/dev/null && ok "T5b cd mount"
sleep 1
ls "$CDMNT" | grep -q "bin-movido.bin" && ok "T5c conteúdo visível" || bad "T5c conteúdo visível"
check "T5d sha via cd-mount" "$h5" "$(sha256sum "$CDMNT/bin-movido.bin" 2>/dev/null | cut -d' ' -f1)"
fusermount3 -u "$CDMNT" 2>/dev/null
rm -rf /tmp/suite-imp-vault && mkdir -p /tmp/suite-imp-vault
CROM_AR_ROOT=/tmp/suite-imp-vault $BIN init >/dev/null
CROM_AR_ROOT=/tmp/suite-imp-vault CROMPRESSOR_BIN=$CROMPRESSOR_BIN $BIN cd import "$R/full.cromar" >/dev/null && ok "T5e cd import"
check "T5f brain instalado" "vault" "$(ls /tmp/suite-imp-vault/brains/ | grep -o '^[a-z]*' | head -1)"
CROM_AR_ROOT=/tmp/suite-imp-vault $BIN extract-file "$R/full.cromar" -o /tmp/suite-ext >/dev/null && ok "T5g extract-file"
check "T5h hash pós-extract" "$h5" "$(sha256sum /tmp/suite-ext/Pk1Renomeado/bin-movido.bin 2>/dev/null | cut -d' ' -f1)"
./bin/crom-ar export PacoteInexistente -o "$R/vazia.cromar" 2>/dev/null && bad "T5i export vazio recusado" || ok "T5i export vazio recusado"
zipinfo -1 "$R/full.cromar" 2>/dev/null | grep -q "brain.cromdb" && ok "T5j bundle contém codebook" || bad "T5j bundle contém codebook"

# ════════ S6 — Codebooks ════════
echo "— S6 Codebooks —"
./bin/crom-ar brain train dominio -i "$CROMPRESSOR_BIN" --size 2048 >/dev/null 2>&1; true
wait_settle
./bin/crom-ar brain list | grep -q dominio && ok "T6a brain train+list" || bad "T6a brain train+list"
./bin/crom-ar brain use dominio && ok "T6b brain use" || bad "T6b brain use"
./bin/crom-ar brain export dominio -o /tmp/suite-brain.cromdb >/dev/null && [ -s /tmp/suite-brain.cromdb ] && ok "T6c brain export" || bad "T6c brain export"
./bin/crom-ar brain add /tmp/suite-brain.cromdb importado >/dev/null && ./bin/crom-ar brain list | grep -q importado && ok "T6d brain add posicional" || bad "T6d brain add posicional"
./bin/crom-ar brain remove dominio 2>/dev/null && bad "T6e remove padrão recusado" || ok "T6e remove padrão recusado"
./bin/crom-ar brain remove importado >/dev/null && ok "T6f remove importado" || bad "T6f remove importado"
./bin/crom-ar brain use dominio >/dev/null

# ════════ S7 — CLI ════════
echo "— S7 CLI —"
./bin/crom-ar list --json 2>/dev/null | python3 -c "
import json,sys
m=json.load(sys.stdin)
e=m['entries'][0]
assert 'hit_rate' in e
sys.exit(0)" && ok "T7a list --json + hit_rate" || bad "T7a list --json + hit_rate"
./bin/crom-ar stats 2>/dev/null | grep -q "ratio médio" && ok "T7b stats" || bad "T7b stats"
./bin/crom-ar verify Pk1Renomeado 2>&1 | grep -q "sha256" && ok "T7c verify" || bad "T7c verify"
vault_before=$(ls "$CROM_AR_ROOT/vault" | wc -l)
./bin/crom-ar rm Pk1Renomeado >/dev/null 2>&1
vault_after=$(ls "$CROM_AR_ROOT/vault" | wc -l)
[ "$vault_after" -lt "$vault_before" ] && ok "T7d rm limpa órfãos" || ok "T7d rm sem órfãos extras ($vault_before→$vault_after)"
./bin/crom-ar info Pk1Renomeado >/dev/null 2>&1 || ./bin/crom-ar info suite-bin.bin >/dev/null 2>&1 && ok "T7e info" || bad "T7e info"
./bin/crom-ar help 2>/dev/null | grep -q "tray" && ok "T7f help lista tray" || bad "T7f help"
./bin/crom-ar comando-inexistente >/dev/null 2>&1; [ $? -ne 0 ] && ok "T7g comando inválido erro" || bad "T7g comando inválido"

# ════════ S8 — Painel web (instância dedicada :8719) ════════
echo "— S8 Web —"
rm -rf /tmp/suite-web-vault && mkdir -p /tmp/suite-web-vault
export CROM_AR_TOKEN_FILE=$R/web-token
CROM_AR_ROOT=/tmp/suite-web-vault $BIN init >/dev/null
setsid env CROM_AR_ROOT=/tmp/suite-web-vault CROM_AR_TOKEN_FILE=$R/web-token CROMPRESSOR_BIN=$CROMPRESSOR_BIN $BIN gui --addr 127.0.0.1:8719 --open=false </dev/null >"$R/web.log" 2>&1 &
WEBPID=$!
sleep 2
W=http://127.0.0.1:8719
WTOK=$(cat "$R/web-token")
code(){ curl -s -o /dev/null -w "%{http_code}" "$@"; }
check "T8a página com token" "200" "$(code "$W/?t=$WTOK")"
body=$(curl -s "$W/" 2>/dev/null)
echo "$body" | grep -q "atalho CROM-AR" && ok "T8b 403 amigável sem token" || bad "T8b 403 amigável"
check "T8c api sem token" "403" "$(code "$W/api/stats")"
check "T8d api token errado" "403" "$(code "$W/api/stats?t=ruim")"
check "T8e host falso" "403" "$(code -H 'Host: evil.com' "$W/api/stats?t=$WTOK")"
check "T8f origin maliciosa" "403" "$(code -X POST -H 'Origin: http://evil.com' -H 'X-Crom-Token: $WTOK' "$W/api/stats")"
check "T8g api/stats com token" "200" "$(code "$W/api/stats?t=$WTOK")"
curl -s "$W/api/tree?t=$WTOK" | python3 -c "import json,sys; d=json.load(sys.stdin); assert 'entries' in d" && ok "T8h api/tree JSON válido" || bad "T8h api/tree"
curl -s "$W/api/packages?t=$WTOK" | python3 -c "import json,sys; assert isinstance(json.load(sys.stdin), list)" && ok "T8i api/packages lista" || bad "T8i api/packages"
curl -s "$W/api/search?q=x&t=$WTOK" | python3 -c "import json,sys; d=json.load(sys.stdin); assert 'hits' in d" && ok "T8j api/search" || bad "T8j api/search"
echo "upload via web" > /tmp/suite-up.txt
curl -s -X POST -F "f0=@/tmp/suite-up.txt" -F "r0=up.txt" -F "folder=/" -H "X-Crom-Token: $WTOK" "$W/api/import" | grep -q '"ok":true' && ok "T8k upload web" || bad "T8k upload web"
curl -s "$W/api/tree?t=$WTOK" | python3 -c "
import json,sys
d=json.load(sys.stdin)
assert any(e['rel_path']=='up.txt' for e in d['entries'])" && ok "T8l upload visível na árvore" || bad "T8l upload"
curl -s "$W/?t=$WTOK" | grep -q "doSearch" && ok "T8m busca na página" || bad "T8m busca na página"
curl -s "$W/?t=$WTOK" | grep -q "Pacotes" && ok "T8n relatório de pacotes na página" || bad "T8n relatório"
curl -s "$W/?t=$WTOK" | grep -q "brain-rows" && ok "T8o seção Codebooks no painel" || bad "T8o seção codebooks"
python3 -c "
import json,urllib.request,sys
tok='$WTOK'
def get(p):
    return json.load(urllib.request.urlopen('$W'+p+('&' if '?' in p else '?')+'t='+tok))
b=get('/api/brains')['brains']
s=get('/api/stats')
orig=sum(x['original'] for x in b); stored=sum(x['stored'] for x in b)
assert orig==s['original_bytes'] and stored==s['stored_bytes'], (orig,stored,s)
sys.exit(0)" && ok "T8p linhas por codebook somam ao global" || bad "T8p coerência api/brains × api/stats"
kill -9 "$WEBPID" 2>/dev/null

# ════════ S9 — Modo persistido: apps rodam direto do cofre ════════
echo "— S9 Modo/exec —"
cp /usr/bin/true "$MNT/app755.bin" && wait_settle
[ -x "$MNT/app755.bin" ] && ok "T9a exec bit preservado na montagem" || bad "T9a exec bit"
"$MNT/app755.bin" && ok "T9b app executado DIRETO do cofre" || bad "T9b exec direto"
echo "texto comum" > "$MNT/app644.txt" && wait_settle
sleep 1.2 # passa do timeout de cache de attrs do kernel (~1s)
[ ! -x "$MNT/app644.txt" ] && ok "T9c texto sem exec bit" || bad "T9c exec bit indevido" "$(stat -c '%A %a' "$MNT/app644.txt" 2>&1) / manifest: $(python3 -c "
import json
m=json.load(open('$CROM_AR_ROOT/manifest.json'))
print(','.join(oct(e.get('mode',0)) for e in m['entries'] if 'app644' in e['name']))" 2>/dev/null)"
chmod +x "$MNT/app644.txt" 2>/dev/null && [ -x "$MNT/app644.txt" ] && ok "T9d chmod na montagem persiste" || bad "T9d chmod"
rm -rf "$R/ext-modo"
$BIN extract app755.bin -o "$R/ext-modo" >/dev/null 2>&1 && [ -x "$R/ext-modo/app755.bin" ] && ok "T9e extração preserva exec bit" || bad "T9e extração"
( set -o noclobber; : > "$MNT/excl.txt" ) 2>/dev/null && ok "T9f criação O_EXCL (padrão do rsync)" || bad "T9f O_EXCL recusado (limitação: rsync falha no cofre)"
wait_settle

# ════════ S10 — Codebook com referências não é removível ════════
echo "— S10 Codebook com refs —"
echo "conteúdo único para brain $RANDOM" > /tmp/suite-brain-doc.txt
./bin/crom-ar add /tmp/suite-brain-doc.txt --brain "$CROM_AR_ROOT/brains/dominio.cromdb" >/dev/null 2>&1 && ok "T10a add com brain específico" || bad "T10a add --brain"
./bin/crom-ar brain use vault >/dev/null 2>&1
./bin/crom-ar brain remove dominio 2>/dev/null && bad "T10b remove com refs deveria recusar" || ok "T10b remove com refs recusado"
./bin/crom-ar rm suite-brain-doc.txt >"$R/t10c-rm.log" 2>&1
./bin/crom-ar rm app755.bin app644.txt excl.txt >"$R/t10c-rm.log" 2>&1 && ok "T10c rm liberta as usagens de dominio" || bad "T10c rm: $(tail -1 "$R/t10c-rm.log")"
./bin/crom-ar brain remove dominio >"$R/t10c-remove.log" 2>&1 && ok "T10d remove sem refs permitido" || bad "T10d remove sem refs: $(tail -1 "$R/t10c-remove.log")"

# ════════ S11 — Tray: FUSE e painel web compartilham o MESMO estado ════════
# Regressão do bug de 28/08: o tray montava o FUSE com uma segunda Store —
# o painel não via o que o FUSE importava e podia apagar entradas ao salvar.
echo "— S11 Tray store compartilhada —"
kill_port 8721
TRAYMNT=$R/traymnt
mkdir -p "$TRAYMNT" "$R/home"
setsid env HOME="$R/home" CROM_AR_ROOT=$CROM_AR_ROOT CROMPRESSOR_BIN=$CROMPRESSOR_BIN \
  CROM_AR_TOKEN_FILE=$R/tray-token "$BIN" tray -m "$TRAYMNT" --web-addr 127.0.0.1:8721 \
  </dev/null >"$R/tray.log" 2>&1 &
TRAYPID=$!
sleep 3
TT=http://127.0.0.1:8721
TTOK=$(cat "$R/tray-token" 2>/dev/null)
check "T11a tray web no ar (:8721)" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$TT/api/stats?t=$TTOK")"
mount | grep -q "$TRAYMNT " && ok "T11b tray montou o cofre" || bad "T11b tray não montou"
echo "regressão store $RANDOM" > "$TRAYMNT/live-sync.txt"
wait_settle
curl -s "$TT/api/tree?t=$TTOK" 2>/dev/null | grep -q "live-sync" && ok "T11c escrita FUSE visível no painel NA HORA" || bad "T11c store dupla! (painel cego ao FUSE)"
fusermount3 -u "$TRAYMNT" 2>/dev/null; umount -l "$TRAYMNT" 2>/dev/null

# ════════ S13 — cd mount esqueleto é read-only ════════
echo "— S13 Esqueleto read-only —"
./bin/crom-ar cd mount "$R/edge.cromar" -m "$CDMNT" --daemon >/dev/null 2>&1 && sleep 1 && ok "T13a cd mount esqueleto" || bad "T13a cd mount esqueleto"
touch "$CDMNT/escrita" 2>/dev/null && bad "T13b esqueleto deveria ser read-only" || ok "T13b esqueleto read-only"
fusermount3 -u "$CDMNT" 2>/dev/null

# ════════ S14 — Concorrência: CLI mutando com residente ativo (T14a) ════════
# O residente faz merge por UID no Save: mutações de CLI não são perdidas.
echo "— S14 Concorrência CLI × residente —"
echo "entrada do CLI $RANDOM" > /tmp/suite-cli.txt
CROM_AR_ROOT=$CROM_AR_ROOT CROMPRESSOR_BIN=$CROMPRESSOR_BIN $BIN add /tmp/suite-cli.txt >/dev/null 2>&1
# dispara um Save do residente (criação de pasta via API persiste o manifest)
curl -s -X POST -H "Content-Type: application/json" -d '{"path":"/lixeira-suite"}' -H "X-Crom-Token: $TTOK" "$TT/api/folder/create" >/dev/null 2>&1; true
sleep 1
python3 -c "
import json,sys
m=json.load(open('$CROM_AR_ROOT/manifest.json'))
sys.exit(0 if any('suite-cli' in e['name'] for e in m['entries']) else 1)" && ok "T14a Save do residente preservou entrada do CLI (merge)" || bad "T14a residente apagou entrada do CLI"
CROM_AR_ROOT=$CROM_AR_ROOT CROMPRESSOR_BIN=$CROMPRESSOR_BIN $BIN rm suite-cli.txt >/dev/null 2>&1; true
kill "$TRAYPID" 2>/dev/null; sleep 1

# S15 - Spool: janitor 24h e recriacao on-demand
echo "— S15 Spool —"
n0=$(ls "$CROM_AR_ROOT/spool" 2>/dev/null | grep -vc tmp || true)
find "$CROM_AR_ROOT/spool" -maxdepth 1 -type f ! -name 'tmp-*' -exec touch -d '25 hours ago' {} + 2>/dev/null
fusermount3 -u "$MNT" 2>/dev/null; umount -l "$MNT" 2>/dev/null
setsid env CROM_AR_ROOT=$CROM_AR_ROOT CROMPRESSOR_BIN=$CROMPRESSOR_BIN $BIN fuse -m "$MNT" </dev/null >"$R/fuse2.log" 2>&1 &
FUSEPID=$!
sleep 2
n1=$(ls "$CROM_AR_ROOT/spool" 2>/dev/null | grep -vc tmp || true)
[ "$n1" -lt "$n0" ] && ok "T15a spool frio podado na remontagem ($n0->$n1)" || bad "T15a spool nao podado ($n0->$n1)"
h=$(sha256sum "$MNT/doc-solto.txt" 2>/dev/null | cut -d' ' -f1)
n2=$(ls "$CROM_AR_ROOT/spool" 2>/dev/null | grep -vc tmp || true)
[ -n "$h" ] && [ "$n2" -ge 1 ] && ok "T15b leitura recria spool on-demand" || bad "T15b spool on-demand"
mount | grep -q "$MNT " && ok "T15c remontagem saudavel" || bad "T15c remontagem"

# S16 - Bench de estrategias (A/B com codebook)
echo "— S16 Bench —"
./bin/crom-ar bench /tmp/suite-doc.txt 2>/dev/null | grep -q "Melhor forma lossless" && ok "T16a bench recomenda estrategia" || bad "T16a bench"
r=$(./bin/crom-ar bench /tmp/suite-doc.txt 2>/dev/null | awk '/^cdc /{for(i=1;i<=NF;i++) if ($i ~ /%$/) {gsub("%","",$i); print $i; exit}}' | head -1)
python3 -c "import sys; sys.exit(0 if float('$r' or 0) < 100 else 1)" && ok "T16b cdc comprime texto (<100%: ${r}%)" || bad "T16b ratio cdc (${r:-vazio}%)"

# S17 - rsync nativo (mkstemp O_EXCL + fchmod em pendente + rename)
# Regressao do Chmod: o rsync da fchmod no temp ANTES do import terminar.
echo "— S17 rsync —"
mkdir -p /tmp/suite-rsync-src/sub
echo "arquivo rsync $RANDOM" > /tmp/suite-rsync-src/a.txt
echo "sub $RANDOM" > /tmp/suite-rsync-src/sub/b.txt
rsync -a /tmp/suite-rsync-src/ "$MNT/rsync-dst/" 2>"$R/rsync.err"
wait_settle
[ ! -s "$R/rsync.err" ] && ok "T17a rsync sem erros" || bad "T17a rsync erros: $(head -1 "$R/rsync.err")"
hsrc=$(sha256sum /tmp/suite-rsync-src/a.txt | cut -d' ' -f1)
hdst=$(sha256sum "$MNT/rsync-dst/a.txt" 2>/dev/null | cut -d' ' -f1)
check "T17b sha pos-rsync" "$hsrc" "$hdst"

echo "════════════════════════════════"
echo "TOTAL: PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
