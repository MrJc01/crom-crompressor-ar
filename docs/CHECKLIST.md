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
23. [ ] Vazio, 1 byte, > RAM (5 GB+), esparso, unicode, caminho longo, symlink, hardlink, permissões, mtime, pasta vazia. Medido em 06/11: `extract` de crom-animate-agent não recria symlinks (5) nem pastas vazias (9); o conteúdo dos arquivos volta idêntico.
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
38. [x] Blocos **parecidos** ("hash + diferença"): bloco novo parecido com um já guardado vira zstd com o parecido como dicionário (`internal/store/delta.go`, features persistidas em `feats.log`, GC mantém as bases). Medido: .wav 101,0 → 72,6 MB; crom-openllm 561,2 → 515,5 MB; studio-scratch 1035,2 → 1011,5 MB (tests/BENCH.md). Futuro: compactar o feats.log no GC (linhas de blocos que saíram ficam).
39. [ ] Criptografia no depósito (hoje `--encrypt` cai no formato `.crom`).
40. [ ] Atualizar `tests/suite.sh` para o depósito e rodar num terminal do sistema; as 27 falhas daqui são todas de FUSE (66 ✔ / 27 ✘, igual antes e depois).

*Pedidos de 06/11: "ganhar através de volume", "e apagar?", "testa com 5 GB, depois 10 GB; tenho pouca memória"*
41. [x] Arquivos pequenos: receitas num log só (`recipes.log`) e manifest comprimido (`manifest.json.zst`, migra o antigo sozinho). Dois projetos reais (201,8 MB): cofre 138,3 → 104,3 MB (1,94×), melhor que o zip (1,73×) — commit `4731856`.
42. [x] Menos RAM: zstd com no máximo 4 workers e modo de pouca memória. Pico do `add` ~1,2 GB → ~450 MB — commit `4731856`.
43. [x] Apagar rápido: `rm` remove as receitas num append só e o GC copia frames quase vivos sem recomprimir. `rm` de um projeto: 43 s → 7,1 s (114 MB de RAM) — commit `4731856`.
44. [x] Corrida no FUSE entre `Release` e o faxineiro (flags atômicas, `-race` limpo) — commit `4731856`.
45. [x] Teste de volume com 4,8 GB (7 projetos reais): cofre 2,12× (2258 MB) × zip 1,70× × 7z por projeto 2,38× × tar.zst único 2,23×. Apagar 1,4 GB: 6,6 s (7z: 164 s). Detalhes em `tests/BENCH.md`.
49. [x] `extract` de 1 MB em 8,8 s: era disco frio + `ionice -c3` no benchmark; o custo real era decodificar o manifest inteiro (0,42 s com 75 mil entradas). Manifest agora lido e gravado em fluxo (entrada por entrada), com teste de que o formato é o mesmo.
50. [x] RAM de pico: manifest em fluxo + limite de memória do Go (384 MB, respeita GOMEMLIMIT). extract 157 → 114 MB, add pequeno 586 → 272 MB, add grande 1,2 GB → 557 MB. Sobra fixa: 4 codificadores zstd (~140 MB); o índice em RAM ainda é pequeno nessa escala (item 9 continua para cofres bem maiores).
51. [x] Arquivo inteiro em zlib (objetos soltos do git, 40% do crom-animate-agent) guardado descomprimido: deduplica com os arquivos do projeto e o nível zlib exato fica na receita (versão 2); a leitura recomprime com a zlib do sistema e confere o SHA-256. Os 3.847 objetos recriados byte a byte, `git fsck` limpo no extraído. Pergunta do usuário "comprime um arquivo por vez?": não — blocos deduplicados entre todos os arquivos, frames de 1 MiB misturando arquivos.
46. [x] Teste de volume com 10 GB: 10,1 GB → cofre 3,35 GB (zip 4,63 GB; 7z por projeto 3,02 GB; tar+zstd -19 único 3,21 GB). RAM 386–444 MB do começo ao fim. Diferença para o 7z vem de repetição distante (frames de 1 MiB) — ver item 47. Detalhes em tests/BENCH.md.
47. [ ] Fechar a diferença para o 7z/LZMA. Parcial: a amostra de compressibilidade olhava só os primeiros 64 KB do frame; agora olha 4 pontos (crom-animate-agent: 624,8 → 613,1 MB). Medido nos frames comprimíveis (435 MB): zstd-11 127 MB, zstd-19 114 MB, xz -9 102 MB; frames de 1 MiB custam só ~1%. Os frames crus são de fato incompressíveis (zstd-11 ganha 4%). Objetos do git guardados descomprimidos (item 51): 613,1 → 556,4 MB (7z 534,2 MB, zip 615,0 MB). Agrupar blocos por tipo e dicionário zstd por tipo: testado, ganho zero (603,7 → 603,1 MB), descartado. Frames maiores (4 e 16 MiB): −3% e −4% no disco, leitura 1,7× e 7× mais lenta, descartado. Blocos parecidos no mesmo frame (minhash): −0,2%, descartado. Frames que ficavam crus porque a amostra de 32 KB não via repetição distante: agora o nível rápido testa o frame inteiro — crom-openllm 632,4 → 561,2 MB (−11%); 10 GB: 3351,6 → 3250,6 MB (tar+zstd --long 3207,6; 7z 3017,4). Delta contra bloco parecido (item 38, feito): crom-openllm 561,2 → 515,5 MB; 10 GB 3250,6 → 3161,2 MB (abaixo do tar+zstd --long 3207,6; 7z 3017,4). Falta: nível mais alto em frames frios (recompactar em segundo plano).
48. [ ] Dois projetos com o mesmo caminho relativo colidem na visão normal (pastas de grupo): decidir como mostrar.

**06/11 — análise do crompressor (pedido: "veja tudo que ainda não está considerando")**
52. [x] Limpar o `feats.log` no GC: reescreve só com blocos vivos e inteiros, sem repetidos, quando há ≥ 25% de lixo (sempre no `doctor --prune`). `internal/store/optimize.go`, teste `TestGCCompactaFeats`.
59. [x] Base órfã ("ocupa os ~16 KB de A só para servir de referência"): todo GC troca a base dos deltas vivos apoiados em bloco que nenhum arquivo usa, por outro bloco vivo parecido, ou guarda o bloco inteiro, quando isso ocupa menos que manter a base. Testes `TestGCTiraBaseOrfa` e `TestGCTrocaBase`. Na linha de comando: v1+v2 (5,7 MB cada, quase iguais) → `rm v1` → depósito 2,45 → 2,30 MB, `verify` ok.
60. [x] Análise de blocos novos contra deltas que já existem (pedido do usuário): (a) delta novo registra as features apontando para a base, então um bloco parecido com um delta acha a mesma base; (b) o mapa de features passa a preferir o bloco mais novo; (c) `Optimize` (roda no `doctor --prune`) lê uma vez os blocos vivos inteiros sem features (depósito antigo, gravado antes dos deltas) e os transforma em delta quando compensa. Teste `TestOptimizeBlocosAntigos` (disco cai > 30%). Falta: medir no cofre real na migração (item 26).
62. [x] Frames frios em zstd 19 (pedido de 06/11, "ok, pode fazer"): recomprimir em segundo plano todo frame já fechado no nível 19 (−10% nos dados comprimíveis, leitura igual). Ler não descompacta nada no disco: descomprime só na memória. Feito: `crom-ar compactar` (rodadas de 256 MB, retomável; codec 2 marca o frame já feito; libzstd em C via cgo). Medido: 1171.6 → 1133.5 MB (−3,3% do cofre), 192 s. Automático ("Sim, automatizar"): o residente (`crom-ar tray`, o do login) checa a cada 5 min e recomprime em rodadas de 64 MB só quando o depósito está sem escrita há 10 min, o load de 1 min está abaixo de 0,3 por CPU e há ≥ 768 MB de RAM livre; para no meio se o PC voltar a trabalhar. Ao terminar, faz um GC leve no máximo a cada 6 h (é a segunda passada que tira blocos como os 29 sem dono, não medido no cofre grande). Desligar: `crom-ar tray --no-compact`. Teste: `TestCompactaEmOcioso` (`internal/vault/idle.go`).
61. [x] Medido (5 GB, 7 projetos): rm 18,8 s e prune 11,2 s, mesma faixa de antes; cofre 2095.7 MB, depois de apagar o studio-scratch 1139.5 MB. Detalhes em `tests/BENCH.md`.
53. [ ] **Decidir (usuário):** `.cromar` local. Hoje é cópia gerada na hora (extrai → tar → treina cérebro do próprio pacote → pack → zip), não fica no cofre. O usuário entende que `.cromar` e depósito ficam os dois locais.
54. [ ] **Decidir (usuário):** sincronização em rede. O `daemon`/`pull` do motor serve arquivos `.crom` soltos de uma pasta e só fala com quem tem o MESMO cérebro (HMAC). O `.cromar` treina um cérebro por pacote → dois pacotes (ou duas versões do mesmo) não sincronizam entre si. Opções: (a) um cérebro fixo por cofre para os `.cromar`; (b) sincronizar depósito ↔ depósito por hash SHA-256 de bloco usando o transporte libp2p do motor (auth, NAT, DHT).
55. [ ] Reaproveitar do motor: encoder zstd 19 em C (`internal/delta/compress_max_cgo.go`, DataDog/zstd) para os frames frios (item 47); AES-256-GCM (`--encrypt`) para cofre cifrado; versão do motor no `meta.json` do `.cromar` (`.crom` novo não abre em binário velho).
57. [ ] Retreino automático (visão do usuário: "a ideia de cromprimir era exatamente ter o retreino automático"). Hoje `train --update` re-escolhe as entradas e invalida `.crom` antigos. Proposta: cérebro que só cresce (ou o próprio depósito como cérebro). Ver `docs/CONCEITOS.md` §6.
58. [ ] Regra do usuário para a rede: blocos nunca passam, só os deltas que montam o bloco do outro lado. Segurança: autorização por pacote, resposta só a quem tem acesso, SHA-256 de cada bloco recebido, versão no protocolo (`docs/CONCEITOS.md` §7).
56. [ ] Subir para o motor a busca de bloco parecido por minhash (`features()` do depósito): a pendência do crompressor diz que a busca aproximada só compara blocos com os mesmos 2 primeiros bytes.

**Futuro (só anotado)**
- LZMA só para frames que quase nunca são lidos (explicação de 06/11): o cofre anota quando cada frame foi lido pela última vez; frame frio e pouco lido vira LZMA (−20% contra o zstd 11, mas leitura 5–10× mais lenta); se um frame em LZMA voltar a ser muito consultado, é regravado em zstd. Depois do zstd 19 (item 62).
- Cofre repartido entre pessoas na rede (cada um guarda partes, acessível a todos). Análise em `docs/CONCEITOS.md` §9; provável projeto separado (crompressor-sinapse), depois do sync cofre ↔ cofre.
- Vídeo entre arquivos diferentes: só com quadros decodificados (pendência do motor). Hoje o ganho em vídeo vem de cópias e trechos idênticos.
- Atualizar o binário em `../crompressor/bin` (outro repositório: decidir lá).

## ▶️ Retomar

1. Feito até aqui: fases 0, 1 e 2 e a leitura sob demanda (fase 3), na branch `deposito-blocos`.
2. Feito em 06/11: limpeza de bases órfãs, feats.log e análise de blocos antigos (itens 52, 59 e 60). Próximo: nível mais alto em frames frios (item 47, os 144 MB que faltam para o 7z). Em seguida `tests/suite.sh` num terminal do sistema (FUSE), a migração do cofre real (item 26) e os itens 18 e 9.
3. Dados de teste: `tests/bench_real.sh` vai baixá-los. Os desta sessão estavam no scratchpad e não ficam.
4. FUSE só testa num terminal do sistema, fora do app Claude (`fusermount3: Operation not permitted`).
6. Conceitos, visão do usuário e decisões abertas: `docs/CONCEITOS.md` (ler antes de mexer em `.cromar`, rede ou cérebro).
5. O residente real está montado em `~/CromAr` (cofre em `~/Documentos/CromAr`): testes sempre com `CROM_AR_ROOT` temporário.
