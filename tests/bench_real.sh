#!/usr/bin/env bash
# Benchmark com dados reais: o cofre economiza disco de verdade?
# Uso: tests/bench_real.sh [binário crom-ar]
#   DADOS=/caminho   cache dos downloads (padrão ~/.cache/crom-ar-bench)
#   SAIDA=arquivo    tabela markdown (padrão tests/BENCH.md)
#   CONJUNTOS="livros bancos versoes videos"   VIDEOS=/pasta/com/videos (opcional)
# Mede o cofre INTEIRO (du de CROM_AR_ROOT: blocos, índices, manifest, brains)
# contra zstd -19 por arquivo e tar + zstd -19 --long (melhor caso de arquivo morto).
set -u
export LC_ALL=C
cd "$(dirname "$0")/.." || exit 1
BIN=$(readlink -f "${1:-bin/crom-ar}")
DADOS=${DADOS:-$HOME/.cache/crom-ar-bench}
SAIDA=${SAIDA:-tests/BENCH.md}
CONJUNTOS=${CONJUNTOS:-livros bancos versoes}
export CROMPRESSOR_BIN=${CROMPRESSOR_BIN:-$PWD/bin/crompressor}
for c in zstd curl tar python3; do command -v $c >/dev/null || { echo "precisa de $c"; exit 1; }; done

baixar() { # url destino
  [ -s "$2" ] || curl -sfL --retry 3 -o "$2.part" "$1" && { [ -s "$2" ] || mv "$2.part" "$2"; }
}

prep_livros() {
  local d=$DADOS/livros; mkdir -p "$d"
  for id in 1342 84 11 1661 2701 98 74 345 1080 2600 5200 76 1952 174 55500 54829 16384 25641 37106 30017 3333 44540 23620 1400 158 46 160 219 829 120; do
    baixar "https://www.gutenberg.org/cache/epub/$id/pg$id.txt" "$d/pg$id.txt" &
  done; wait
}
prep_bancos() {
  local d=$DADOS/bancos; mkdir -p "$d"
  local B=https://github.com/lerocha/chinook-database/raw/master/ChinookDatabase/DataSources
  for f in Chinook_Sqlite.sqlite Chinook_PostgreSql.sql Chinook_MySql.sql Chinook_SqlServer.sql Chinook_Oracle.sql Chinook_Sqlite.sql; do
    baixar "$B/$f" "$d/$f" &
  done
  baixar https://github.com/bradleygrant/sakila-sqlite3/raw/main/sakila_master.db "$d/sakila.db" &
  baixar https://github.com/jpwhite3/northwind-SQLite3/raw/main/dist/northwind.db "$d/northwind.db" &
  wait
}
prep_versoes() { # mesmo banco alterado aos poucos + versões de um tar de código
  local d=$DADOS/versoes; mkdir -p "$d"; prep_bancos
  [ -s "$d/northwind-v1.db" ] || cp "$DADOS/bancos/northwind.db" "$d/northwind-v1.db"
  for i in 2 3 4; do
    [ -s "$d/northwind-v$i.db" ] && continue
    cp "$d/northwind-v$((i-1)).db" "$d/northwind-v$i.db"
    python3 -I -c "
import sqlite3,sys;c=sqlite3.connect(sys.argv[1])
c.execute('update Orders set ShipCity=ShipCity||\"x\" where OrderID%97=0')
c.execute('insert into Categories(CategoryName,Description) values (\"Nova\",\"categoria nova\")');c.commit()" "$d/northwind-v$i.db"
  done
  if [ ! -s "$d/codigo-v1.tar" ]; then # 5 versões deste próprio repositório
    local n=0 h
    for h in $(git log --format=%h -n 15 | awk 'NR%3==1'); do
      n=$((n+1)); git archive --format=tar "$h" > "$d/codigo-v$n.tar"
    done
  fi
}
prep_videos() {
  local d=$DADOS/videos; mkdir -p "$d"
  [ -n "${VIDEOS:-}" ] || { echo "  (videos: defina VIDEOS=/pasta)"; return 1; }
  find "$VIDEOS" -maxdepth 3 -type f \( -name '*.mp4' -o -name '*.webm' -o -name '*.mkv' \) -size -60M | head -5 |
    while read -r f; do cp -n "$f" "$d/"; done
  local first; first=$(ls "$d" | grep -v COPIA | head -1)
  [ -n "$first" ] && cp -n "$d/$first" "$d/COPIA-$first"   # cópia idêntica: dedup tem que pegar
}

mb() { awk -v b="$1" 'BEGIN{printf "%.2f", b/1048576}'; }
{
  echo "# Benchmark com dados reais"
  echo
  echo "$(date '+%Y-%m-%d %H:%M') · $(basename "$BIN") · motor $("$CROMPRESSOR_BIN" --version 2>/dev/null | awk '{print $NF}')"
  echo
  echo "| conjunto | arquivos | original MB | cofre MB | zstd -19 MB | tar+zstd --long MB | cofre/zstd | import s | roundtrip |"
  echo "|---|---|---|---|---|---|---|---|---|"
} > "$SAIDA.tmp"

for c in $CONJUNTOS; do
  echo "→ $c"; prep_$c || continue
  d=$DADOS/$c
  R=$(mktemp -d /tmp/crom-bench-XXXX); export CROM_AR_ROOT=$R/cofre
  orig=$(du -sb "$d" | cut -f1); n=$(ls "$d" | wc -l)
  t0=$(date +%s.%N)
  "$BIN" add "$d" > "$R/add.log" 2>&1 || echo "  ! add falhou (veja $R/add.log)"
  t=$(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b-a}')
  rm -rf "$CROM_AR_ROOT/spool"
  cofre=$(du -sb "$CROM_AR_ROOT" | cut -f1)
  z=0; for f in "$d"/*; do z=$((z + $(zstd -19 -q -c "$f" | wc -c))); done
  tz=$(tar cf - -C "$d" . | zstd -19 --long=27 -T0 -q -c | wc -c)
  # roundtrip: extrai tudo e confere sha256 arquivo a arquivo
  "$BIN" extract "$c" -o "$R/out" --force > "$R/extract.log" 2>&1
  ok=0; tot=0
  for f in "$d"/*; do
    tot=$((tot+1)); g=$(find "$R/out" -name "$(basename "$f")" -type f | head -1)
    [ -n "$g" ] && [ "$(sha256sum < "$f")" = "$(sha256sum < "$g")" ] && ok=$((ok+1))
  done
  ratio=$(awk -v a="$cofre" -v b="$z" 'BEGIN{printf "%.2f×", a/b}')
  echo "| $c | $n | $(mb $orig) | **$(mb $cofre)** | $(mb $z) | $(mb $tz) | $ratio | $t | $ok/$tot |" >> "$SAIDA.tmp"
  tail -1 "$SAIDA.tmp"
  [ "${KEEP:-0}" = 1 ] || rm -rf "$R"
done
mv "$SAIDA.tmp" "$SAIDA"
echo "tabela em $SAIDA"
