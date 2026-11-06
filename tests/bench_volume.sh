#!/usr/bin/env bash
# Teste de VOLUME: vários projetos reais no mesmo cofre × um zip/7z por projeto
# × tar+zstd --long de tudo junto. Mede crescimento a cada projeto, custo de
# apagar um projeto e de abrir um arquivo do meio.
#
# Pensado para PC com pouca RAM: 7z e zstd com 2 threads, um arquivo de
# comparação por vez (apagado logo depois de medido).
#
#   tests/bench_volume.sh                      # projetos padrão (~5 GB)
#   ORIGEM=~/x PROJETOS="a b c" tests/bench_volume.sh
#   TRAB=/outro/disco SAIDA=resultado.md tests/bench_volume.sh
set -euo pipefail
export LC_ALL=C

AQUI="$(cd "$(dirname "$0")/.." && pwd)"
CROM="$AQUI/bin/crom-ar"
ORIGEM="${ORIGEM:-$HOME/Documentos/GitHub}"
PROJETOS="${PROJETOS:-opencorp opencorp-claude crom-animate-agent crom-animate-studio-scratch crompressor crompressor-stream crom-animacoes}"
APAGAR="${APAGAR:-crom-animate-studio-scratch}" # projeto removido no teste de apagar
TRAB="${TRAB:-$(mktemp -d "${TMPDIR:-/tmp}/crom-vol.XXXXXX")}"
SAIDA="${SAIDA:-$TRAB/resultado.md}"
mkdir -p "$TRAB"
export CROM_AR_ROOT="$TRAB/cofre"

mb() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }
du_b() { du -sb "$1" | cut -f1; }
agora() { date +%s.%N; }
dt() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }
# pico de RAM (KB) do último comando rodado com medir
medir() { /usr/bin/time -f "%M" -o "$TRAB/.rss" "$@"; }
rss_mb() { awk '{printf "%.0f", $1/1024}' "$TRAB/.rss"; }

{
  echo "# Teste de volume — $(date '+%d/%m/%Y %H:%M')"
  echo
  echo "Origem: \`$ORIGEM\`. Cofre: disco inteiro (\`du\` da pasta do cofre)."
  echo
  echo "## Crescimento do cofre a cada projeto"
  echo
  echo "| projeto | original | arquivos | cofre acumulado | cofre +projeto | tempo | RAM pico | zip -9 | 7z -mx9 sólido |"
  echo "|---|---|---|---|---|---|---|---|---|"
} >"$SAIDA"

tot_orig=0; tot_zip=0; tot_7z=0; prev=0
for p in $PROJETOS; do
  src="$ORIGEM/$p"
  [ -d "$src" ] || { echo "pulando $p (não existe)"; continue; }
  orig=$(find "$src" -type f -printf '%s\n' | awk '{s+=$1}END{print s+0}')
  nf=$(find "$src" -type f | wc -l)
  echo ">> $p: $(mb "$orig") MB, $nf arquivos"
  t0=$(agora)
  medir "$CROM" add "$src" >"$TRAB/add-$p.log" 2>&1
  t1=$(agora); rss=$(rss_mb)
  cur=$(du_b "$CROM_AR_ROOT")
  # comparação: um arquivo compactado por projeto (como se faz no dia a dia)
  (cd "$ORIGEM" && zip -q -r -9 -y "$TRAB/p.zip" "$p")
  z=$(stat -c %s "$TRAB/p.zip"); rm -f "$TRAB/p.zip"
  t7=$(agora)
  (cd "$ORIGEM" && 7z a -bd -bso0 -mx=9 -ms=on -mmt=2 -snl "$TRAB/$p.7z" "$p" >/dev/null)
  t7b=$(agora)
  s7=$(stat -c %s "$TRAB/$p.7z")
  [ "$p" = "$APAGAR" ] && echo "$(dt "$t7" "$t7b")" >"$TRAB/.t7-$p" || rm -f "$TRAB/$p.7z"
  tot_orig=$((tot_orig + orig)); tot_zip=$((tot_zip + z)); tot_7z=$((tot_7z + s7))
  echo "| $p | $(mb "$orig") MB | $nf | **$(mb "$cur") MB** | +$(mb $((cur - prev))) MB | $(dt "$t0" "$t1") s | $rss MB | $(mb "$z") MB | $(mb "$s7") MB |" >>"$SAIDA"
  prev=$cur
done

echo ">> tar + zstd -19 --long=27 de tudo junto"
t0=$(agora)
tzst=$( (cd "$ORIGEM" && tar cf - $PROJETOS 2>/dev/null) | zstd -q -19 --long=27 -T2 -c | wc -c)
t1=$(agora)
cofre=$(du_b "$CROM_AR_ROOT")
{
  echo "| **total** | **$(mb "$tot_orig") MB** | | **$(mb "$cofre") MB** | | | | $(mb "$tot_zip") MB | $(mb "$tot_7z") MB |"
  echo
  echo "tar + zstd -19 --long=27 de tudo num arquivo só: **$(mb "$tzst") MB** ($(dt "$t0" "$t1") s)."
  echo
  echo "Razão (original ÷ armazenado): cofre $(awk -v a="$tot_orig" -v b="$cofre" 'BEGIN{printf "%.2f", a/b}')×," \
       "zip $(awk -v a="$tot_orig" -v b="$tot_zip" 'BEGIN{printf "%.2f", a/b}')×," \
       "7z por projeto $(awk -v a="$tot_orig" -v b="$tot_7z" 'BEGIN{printf "%.2f", a/b}')×," \
       "tar.zst único $(awk -v a="$tot_orig" -v b="$tzst" 'BEGIN{printf "%.2f", a/b}')×."
  echo
  echo '```'
  "$CROM" stats 2>&1 | sed -n '/DISCO REAL/,$p'
  echo '```'
} >>"$SAIDA"

# ── abrir um arquivo do meio ───────────────────────────────────────────────
echo ">> abrir um arquivo"
py=$(command -v python3)
alvo=$("$CROM" list --json | "$py" -I -c '
import json,sys
es=[e for e in json.load(sys.stdin)["entries"] if e.get("group")==sys.argv[1] and e.get("size",0)>0]
es.sort(key=lambda e:-e["size"])
e=es[len(es)//50] if es else None   # um dos maiores, não o maior
print(e["id"], e["size"], e["rel_path"]) if e else None' "$APAGAR")
read -r aid asz arel <<<"$alvo"
t0=$(agora)
"$CROM" extract "$aid" -o "$TRAB/x-cofre" >/dev/null
t1=$(agora)
t2=$(agora)
(cd "$TRAB" && 7z e -bd -bso0 -y -o"$TRAB/x-7z" "$APAGAR.7z" "$APAGAR/$arel" >/dev/null) || true
t3=$(agora)
{
  echo
  echo "## Abrir um arquivo de $(mb "$asz") MB de dentro de \`$APAGAR\`"
  echo
  echo "| | tempo |"
  echo "|---|---|"
  echo "| cofre (\`crom-ar extract\`) | $(dt "$t0" "$t1") s |"
  echo "| 7z sólido (\`7z e\` de um arquivo) | $(dt "$t2" "$t3") s |"
} >>"$SAIDA"

# ── apagar um projeto ─────────────────────────────────────────────────────
echo ">> apagar $APAGAR"
antes=$(du_b "$CROM_AR_ROOT")
t0=$(agora)
medir "$CROM" rm "$APAGAR" >/dev/null
t1=$(agora); rss=$(rss_mb)
depois=$(du_b "$CROM_AR_ROOT")
t2=$(agora)
"$CROM" doctor --prune >"$TRAB/prune.log" 2>&1 || true
t3=$(agora)
podado=$(du_b "$CROM_AR_ROOT")
# no 7z: apagar metade dos arquivos do projeto obriga a recomprimir o bloco sólido
s7a=$(stat -c %s "$TRAB/$APAGAR.7z")
t4=$(agora)
(cd "$TRAB" && 7z d -bd -bso0 -mmt=2 "$APAGAR.7z" "$APAGAR/*.json" -r >/dev/null) || true
t5=$(agora)
s7b=$(stat -c %s "$TRAB/$APAGAR.7z")
{
  echo
  echo "## Apagar o projeto \`$APAGAR\` do cofre"
  echo
  echo "| | cofre | tempo | RAM pico |"
  echo "|---|---|---|---|"
  echo "| antes | $(mb "$antes") MB | | |"
  echo "| \`crom-ar rm\` (GC automático) | $(mb "$depois") MB (−$(mb $((antes - depois))) MB) | $(dt "$t0" "$t1") s | $rss MB |"
  echo "| \`crom-ar doctor --prune\` | $(mb "$podado") MB (−$(mb $((antes - podado))) MB no total) | $(dt "$t2" "$t3") s | |"
  echo
  echo "Para comparar: \`7z d\` dos .json de dentro do .7z sólido desse projeto" \
       "($(mb "$s7a") → $(mb "$s7b") MB) levou $(dt "$t4" "$t5") s (o 7z regrava o arquivo)."
} >>"$SAIDA"

rm -rf "$TRAB/x-cofre" "$TRAB/x-7z"
echo
cat "$SAIDA"
echo
echo "Pasta de trabalho: $TRAB (apague quando terminar)"
