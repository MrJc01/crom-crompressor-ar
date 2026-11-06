# Checklist crom-ar

Plano detalhado: [PLANO.md](PLANO.md). Medições: [DIAGNOSTICO-armazenamento.md](DIAGNOSTICO-armazenamento.md).

## 📥 Pedidos de 2026-11-05

> "alguns dados não estão sendo bem comprimidos… se eu pegar vários textos e
> colocar não parece ter economia e acesso ao dado corretamente"
> "testar com dados reais… baixar banco de dados… saber se diminui o armazenamento"
> "deve funcionar para todos os arquivos como planejamos, vídeos etc."

**Feito**
- [x] Analisar o objetivo do projeto e testar com dados reais: 30 livros, 4 bancos SQLite, 5 dumps SQL, 9 versões (crom-tv + Northwind) e 5 vídeos reais. Resultado em `docs/DIAGNOSTICO-armazenamento.md`.
- [x] Diagnóstico: o cofre ficava pior que o zstd, não tinha dedup entre versões, arquivos ≥ 18 MB falhavam no motor V24 e a leitura pelo FUSE duplicava o arquivo no `spool/`.
- [x] Protótipo do depósito de blocos: versões 242,8 MB → 69,5 MB; livros 6,9 → 5,6 MB; bancos 18,3 → 11,3 MB; roundtrip SHA-256 ok.
- [x] Trocar o motor V24 pelo novo em `bin/` (o antigo ficou em `bin/crompressor.v24-antigo`); `go.mod` → `../crompressor`; `go test ./...` ok.
- [x] `make pull-bin` atualiza o motor quando ele muda (antes nunca atualizava).
- [x] Achado: o binário de `../crompressor/bin` é mais velho que o fonte (falta `f88b72f`). Compilado do fonte, o cérebro guarda todos os blocos (versão nova do Northwind: 7,6 MB → 22 KB).

**A fazer — ordem de prioridade**

*Decisão primeiro*
1. [x] **D1 — onde ficam os blocos: depósito próprio do cofre** (decidido pelo usuário em 05/11). Adendo do usuário: "o crompressor é só a base do nosso sistema cromar". O depósito nasce no crom-ar; o crompressor segue como base (`.cromar`, sincronização, cérebro), e o que ficar genérico pode subir para o motor depois.

*Fase 0 — motor certo e medição honesta*
2. [x] `make build` compila o motor a partir do fonte `../crompressor` — commit `3bb1e88`.
3. [ ] `doctor` avisa quando o motor do cofre é antigo.
4. [ ] parcial: `stats` mostra o DISCO REAL (depósito + .crom + codebooks + manifest; spool à parte). Falta o painel web.
5. [x] `tests/bench_real.sh` + `tests/BENCH.md` (livros, bancos, versões, vídeos; roundtrip 52/52).
6. [x] DIAGNOSTICO corrigido com o motor compilado do fonte.

*Fase 1 — formato do depósito (`internal/store`)*
7. [x] FastCDC 4/16/64 KB + SHA-256 por bloco (`internal/store/cdc.go`). Testado: 4 KB médio só melhora 6% nas versões; ficou 16 KB.
8. [x] Segmentos append-only (256 MB), frames de 1 MiB em zstd, CRC32C por frame (`format.go`, `store.go`).
9. [ ] parcial: `index.log` append-only em disco, mas carregado num mapa em RAM (~100 B por bloco ≈ 650 MB para 100 GB). Falta índice paginado (bbolt ou ordenado em disco).
10. [x] Receita por SHA-256 do conteúdo, com CRC (`recipes/ab/<sha>.rcp`).
11. [x] GC por marcação a partir das receitas; roda sozinho ao remover (segmentos ≥30% mortos) e em `doctor --prune`; `doctor` conta blocos órfãos sem apagar.
12. [x] Ordem blocos → índice → receita, com fsync; índice cortado por queda é truncado; GC por rename atômico. Testes: índice cortado, frame corrompido, receita corrompida. Falta o teste de `kill -9` real (item 36).
13. [x] `store/FORMAT` + versão na receita + magic nos frames.

*Fase 2 — importar, extrair, verificar*
14. [x] `add` em streaming (sem carregar o arquivo), verifica bloco a bloco + SHA-256 antes de registrar. `--crom` mantém o formato antigo.
15. [x] Por frame, sem depender da extensão: amostra rápida + zstd; frame que não comprime (mp4, jpg, zip, aleatório) fica cru. Medido: vídeos 44,5 MB → 31,9 MB (cópia idêntica de graça).
16. [ ] SQLite: comparar corte em páginas de 4 KB contra CDC.
17. [x] `extract`, `verify`, `info`, `rm`, `doctor`, export `.cromar` e `grep` do painel (grep completo, lendo o conteúdo) pelo depósito.
18. [ ] parcial: flock entre processos + trava de escrita separada da leitura (ler pelo FUSE não espera import). Testado com duas instâncias e `-race`. Falta comprimir arquivos pequenos em paralelo (hoje ~6 MB/s em textos).

*Fase 3 — leitura sob demanda*
19. [x] FUSE lê pelo `ReadAt` do depósito, cache LRU de 64 MB. Testado chamando o filesystem direto (sem montar): `internal/fusefs/blocks_test.go`. Falta testar montado (item 33).
20. [x] Leitura não usa mais o `spool/`, e o arquivo escrito pela montagem não fica mais duplicado no `spool/` depois de importar. `Truncate` não altera mais o conteúdo compartilhado (bug antigo, com teste).
21. [ ] Vídeo tocando direto da montagem com avanço/retrocesso (mpv/VLC).
22. [ ] Apps executando do cofre; SQLite aberto direto do cofre (somente leitura).

*Fase 4 — todos os arquivos e casos de borda*
23. [ ] Vazio, 1 byte, > RAM (5 GB+), esparso, unicode, caminho longo, symlink, hardlink, permissões, mtime, pasta vazia.
24. [ ] 100 mil arquivos pequenos (`node_modules`).
25. [ ] Disco cheio e arquivo alterado durante o import.

*Fase 5 — migração*
26. [ ] `crom-ar migrate` com backup antes e SHA-256 por entrada (cofre real: 187 entradas, 6,9 MB).

*Fase 6 — ecossistema CROM*
27. [x] `.cromar` sem codebook no cofre treina um com o próprio pacote (`-k 4096`); roundtrip conferido (`bundle_test.go`). **Bug corrigido:** o `cd import` usava `Fuzziness>0` e trocava trechos entre arquivos (decodificação com perda do motor); agora confere o SHA-256 de cada membro.
28. [ ] Sincronizar dois cofres só com os blocos que faltam; medir depósito vs. `daemon`/`pull`.
29. [ ] Revisar `brain`, `bench` e `edge`/esqueleto com o depósito: manter, adaptar ou remover.

*Fase 7 — painel e docs*
30. [ ] Painel: disco real, economia por tipo, blocos divididos entre arquivos, progresso de import/GC/migração.
31. [ ] README reescrito sem as regras do V24.

*Fase 8 — testes e aceite*
32. [x] 14 testes do depósito + 4 do FUSE sem montar + 1 do `.cromar`, todos com `-race`.
33. [ ] `tests/suite.sh` adaptada; rodar num terminal do sistema (FUSE não monta de dentro do app Claude).
34. [ ] `bench_real.sh` batendo as metas da tabela do PLANO.
35. [ ] Escala: 10 GB variados, 1 arquivo de 5 GB, 100 mil arquivos.
36. [ ] Durabilidade: `kill -9` no import e no GC, depois `doctor` limpo e SHA-256 ok.

*Novos itens (06/11)*
37. [x] `fsync` em lote no `add` de pasta: um sync por lote e frames juntando arquivos — commit `cd6077c`.
38. [ ] Blocos **parecidos** ("hash + diferença"): comprimir o bloco novo usando o parecido como dicionário. Fecharia a diferença para o tar+zstd --long nas versões (11,15 MB vs 7,53 MB).
39. [ ] Criptografia no depósito (hoje `--encrypt` cai no formato `.crom`).
40. [ ] Atualizar `tests/suite.sh` para o depósito e rodar num terminal do sistema; as 27 falhas daqui são todas de FUSE (66 ✔ / 27 ✘, igual antes e depois).

*Pedidos de 06/11: "ganhar através de volume", "e apagar?", "testa com 5 GB, depois 10 GB; tenho pouca memória"*
41. [x] Arquivos pequenos: receitas num log só (`recipes.log`) e manifest comprimido (`manifest.json.zst`, migra o antigo sozinho). Dois projetos reais (201,8 MB): cofre 138,3 → 104,3 MB (1,94×), melhor que o zip (1,73×) — commit `4731856`.
42. [x] Menos RAM: zstd com no máximo 4 workers e modo de pouca memória. Pico do `add` ~1,2 GB → ~450 MB — commit `4731856`.
43. [x] Apagar rápido: `rm` remove as receitas num append só e o GC copia frames quase vivos sem recomprimir. `rm` de um projeto: 43 s → 7,1 s (114 MB de RAM) — commit `4731856`.
44. [x] Corrida no FUSE entre `Release` e o faxineiro (flags atômicas, `-race` limpo) — commit `4731856`.
45. [x] Teste de volume com 4,8 GB (7 projetos reais): cofre 2,12× (2258 MB) × zip 1,70× × 7z por projeto 2,38× × tar.zst único 2,23×. Apagar 1,4 GB: 6,6 s (7z: 164 s). Detalhes em `tests/BENCH.md`.
49. [ ] `extract` de um arquivo de 1 MB levou 8,8 s num cofre de 2,2 GB: abrir não pode depender do tamanho do cofre.
50. [ ] RAM de pico cresce com o cofre (1,2 GB com 2,2 GB de cofre): resolver com o item 9 (índice fora da RAM) antes do teste de 10 GB.
46. [ ] Teste de volume com 10 GB (depois do de 5 GB, pedido do usuário).
47. [ ] Fechar a diferença para o 7z/LZMA: codec mais forte por frame (zstd -19/níveis altos em frames frios, ou xz) e o item 38 (blocos parecidos).
48. [ ] Dois projetos com o mesmo caminho relativo colidem na visão normal (pastas de grupo): decidir como mostrar.

**Futuro (só anotado)**
- Vídeo entre arquivos diferentes: só com quadros decodificados (pendência do motor). Hoje o ganho em vídeo vem de cópias e trechos idênticos.
- Atualizar o binário em `../crompressor/bin` (outro repositório: decidir lá).

## ▶️ Retomar

1. Feito até aqui: fases 0, 1 e 2 e a leitura sob demanda (fase 3), na branch `deposito-blocos`.
2. Próximo: itens 49 e 50 (abrir rápido, RAM com índice fora da memória), depois o teste de 10 GB (46) e o codec mais forte (47). Em seguida `tests/suite.sh` num terminal do sistema (FUSE), a migração do cofre real (item 26) e os itens 18 e 9.
3. Dados de teste: `tests/bench_real.sh` vai baixá-los. Os desta sessão estavam no scratchpad e não ficam.
4. FUSE só testa num terminal do sistema, fora do app Claude (`fusermount3: Operation not permitted`).
5. O residente real está montado em `~/CromAr` (cofre em `~/Documentos/CromAr`): testes sempre com `CROM_AR_ROOT` temporário.
