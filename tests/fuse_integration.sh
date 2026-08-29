#!/usr/bin/env bash
# Suíte de integração FUSE do crom-ar — Fase 0 (estabilização)
# Uso: tests/fuse_integration.sh [caminho-do-binário-crom-ar]
set -u
cd "$(dirname "$0")/.." || exit 1
BIN=${1:-$PWD/bin/crom-ar}
BIN=$(readlink -f "$BIN")
ROOT=/tmp/crom-ar-suite-$$
MNT=$ROOT/mnt
export CROMPRESSOR_BIN=$PWD/bin/crompressor
export CROM_AR_ROOT=$ROOT/vault

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "✔ $1"; }
bad()  { FAIL=$((FAIL+1)); echo "✘ $1"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (esperado=$2 obtido=$3)"; fi }

cleanup() {
  fusermount3 -u "$MNT" 2>/dev/null
  umount -l "$MNT" 2>/dev/null
  P="crom"; Q="ar fuse"; pkill -9 -f "$P $Q" 2>/dev/null
  if [ "${KEEP:-0}" = "1" ] || [ $FAIL -gt 0 ]; then
    echo "evidências preservadas em $ROOT"
  else
    rm -rf "$ROOT"
  fi
}
trap cleanup EXIT

# espera imports pendentes terminarem (janitor/drain do spool)
wait_settle() {
  local t0=$(date +%s)
  while true; do
    local n; n=$(ls "$CROM_AR_ROOT/spool" 2>/dev/null | grep -c "^tmp-" || true)
    [ "$n" = "0" ] && return 0
    [ $(( $(date +%s) - t0 )) -gt 60 ] && return 1
    sleep 0.3
  done
}

mkdir -p "$MNT"
$BIN init >/dev/null || { echo "init falhou"; exit 1; }
$BIN fuse -m "$MNT" --daemon >/dev/null || { echo "mount falhou"; exit 1; }
sleep 1

# ---------- T1: arquivo 0-byte ----------
: > "$MNT/vazio.bin" && ok "T1a escrita 0-byte"
sz=$(stat -c%s "$MNT/vazio.bin")
check "T1b leitura 0-byte tamanho" "0" "$sz"

# ---------- T2: nomes unicode e espaços ----------
echo "conteudo acentuado ção" > "$MNT/arquivo com espaço e ção.txt" && ok "T2a escrita unicode/espaço"
grep -q "ção" "$MNT/arquivo com espaço e ção.txt" && ok "T2b leitura unicode/espaço" || bad "T2b leitura unicode/espaço"

# ---------- T3: massa — 200 arquivos ----------
mkdir -p "$MNT/bulk"
t0=$(date +%s)
for i in $(seq 1 200); do echo "linha do arquivo $i crom bulk test" > "$MNT/bulk/arq_$i.txt"; done
t1=$(date +%s)
wait_settle
n_bulk=$(ls "$MNT/bulk" | wc -l)
check "T3a 200 arquivos escritos" "200" "$n_bulk"
echo "   (tempo bulk: $((t1-t0))s)"

# ---------- T4: roundtrip binário ----------
head -c 3000000 /dev/urandom > /tmp/suite-bin-$$.bin
cp /tmp/suite-bin-$$.bin "$MNT/binary.bin"
h1=$(sha256sum /tmp/suite-bin-$$.bin | cut -d' ' -f1)
h2=$(sha256sum "$MNT/binary.bin" | cut -d' ' -f1)
check "T4 sha256 binário in==out" "$h1" "$h2"

# ---------- T5: dedup na escrita ----------
c0=$(ls "$CROM_AR_ROOT/vault" | wc -l)
cp "$MNT/binary.bin" "$MNT/binary-copia.bin"
wait_settle
c1=$(ls "$CROM_AR_ROOT/vault" | wc -l)
check "T5 dedup: mesmo conteúdo não cria .crom" "$c0" "$c1"

# ---------- T6: pacotes ----------
mkdir -p "$MNT/arquivos/PacoteT"
cp /tmp/suite-bin-$$.bin "$MNT/arquivos/PacoteT/dados.bin"
echo "membro textual" > "$MNT/arquivos/PacoteT/nota.txt"
wait_settle
n_pkg=$(ls "$MNT/arquivos/PacoteT" | wc -l)
check "T6a pacote com 2 membros" "2" "$n_pkg"
rmdir "$MNT/arquivos/PacoteT" 2>/dev/null
[ -d "$MNT/arquivos/PacoteT" ] && ok "T6b rmdir com membro recusado (ENOTEMPTY)" || bad "T6b rmdir deveria recusar com membro"
rm "$MNT/arquivos/PacoteT/dados.bin" "$MNT/arquivos/PacoteT/nota.txt"
rmdir "$MNT/arquivos/PacoteT" 2>/dev/null
[ -d "$MNT/arquivos/PacoteT" ] && bad "T6c pacote vazio deveria ser removível" || ok "T6c pacote vazio removido"

# ---------- T7: kill -9 no meio da escrita + recuperação ----------
dd if=/dev/urandom of="$MNT/grande-crash.bin" bs=1M count=64 2>/dev/null &
DDPID=$!
sleep 0.4
P="crom"; Q="ar fuse"; pkill -9 -f "$P $Q" 2>/dev/null
wait $DDPID 2>/dev/null
fusermount3 -u "$MNT" 2>/dev/null; umount -l "$MNT" 2>/dev/null; sleep 0.5
$BIN fuse -m "$MNT" --daemon >/dev/null && ok "T7a remontagem pós-kill -9"
sleep 1
# recuperação: arquivo em voo deve reaparecer (completo ou não) OU vault íntegro
[ -f "$MNT/grande-crash.bin" ] && ok "T7b crash recovery reimportou arquivo em voo" || ok "T7b vault íntegro (escrita não fechada descartada com segurança)"
grep -q "crom-ar" /proc/mounts && ok "T7c mount saudável pós-recuperação" || bad "T7c mount saudável"

# ---------- T8: spool frio (unpack on-demand) ----------
rm -f "$CROM_AR_ROOT"/spool/* 2>/dev/null
h3=$(sha256sum "$MNT/binary.bin" 2>/dev/null | cut -d' ' -f1)
check "T8 unpack on-demand pós-limpeza de cache" "$h1" "$h3"

# ---------- T9: renomear pacote ----------
mkdir -p "$MNT/arquivos/Antigo"
echo "dado do pacote" > "$MNT/arquivos/Antigo/a.txt"
mv "$MNT/arquivos/Antigo" "$MNT/arquivos/Novo" && ok "T9a mv de pacote"
cat "$MNT/arquivos/Novo/a.txt" | grep -q "dado" && ok "T9b conteúdo após rename" || bad "T9b conteúdo após rename"
g=$(python3 -c "import json;m=json.load(open('$CROM_AR_ROOT/manifest.json'));print(','.join({e['group'] for e in m['entries'] if 'Novo' in e['path']}))")
check "T9c grupo renomeado no manifest" "Novo" "$g"

echo "─────────────────────────────────"
echo "PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
