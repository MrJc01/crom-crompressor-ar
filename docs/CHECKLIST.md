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
2. [ ] `make build` compila o motor a partir do fonte `../crompressor` (e não copia o binário velho).
3. [ ] `doctor` avisa quando o motor do cofre é antigo.
4. [ ] `stats` e painel contam `brains/`, `spool/`, índices e manifest no disco.
5. [ ] `tests/bench_real.sh` + `tests/BENCH.md`: dados reais vs. zstd e tar+zstd --long.
6. [ ] Corrigir o DIAGNOSTICO com o resultado do motor compilado do fonte.

*Fase 1 — formato do depósito (`internal/store`)*
7. [ ] FastCDC (4/16/64 KB) + hash de 32 bytes por bloco.
8. [ ] Segmentos append-only de ~1 MB em zstd, checksum por frame.
9. [ ] Índice hash → posição em disco (sem exigir tudo na RAM).
10. [ ] Receita do arquivo (lista de blocos), pela SHA-256 do arquivo.
11. [ ] Referências por bloco + GC/compactação de segmentos (`doctor --prune`).
12. [ ] Durabilidade: tmp + fsync + rename + journal; import interrompido não corrompe.
13. [ ] Versão do formato no cabeçalho.

*Fase 2 — importar, extrair, verificar*
14. [ ] `add` em streaming com verificação SHA-256 antes de confirmar.
15. [ ] Detecção por tipo: texto/SQL/código → zstd alto; já comprimido (mp4, jpg, zip…) → só dedup, sem zstd; aleatório → cru.
16. [ ] SQLite: comparar corte em páginas de 4 KB contra CDC.
17. [ ] `extract`, `verify`, `info`, `rm` pelo depósito.
18. [ ] Concorrência: imports paralelos e CLI + residente com lock no depósito.

*Fase 3 — leitura sob demanda*
19. [ ] FUSE `Read(offset, n)` só descomprime os frames do trecho, com cache LRU em RAM.
20. [ ] Remover o `spool/` da leitura (escrita continua em tmp até o RELEASE).
21. [ ] Vídeo tocando direto da montagem com avanço/retrocesso (mpv/VLC).
22. [ ] Apps executando do cofre; SQLite aberto direto do cofre (somente leitura).

*Fase 4 — todos os arquivos e casos de borda*
23. [ ] Vazio, 1 byte, > RAM (5 GB+), esparso, unicode, caminho longo, symlink, hardlink, permissões, mtime, pasta vazia.
24. [ ] 100 mil arquivos pequenos (`node_modules`).
25. [ ] Disco cheio e arquivo alterado durante o import.

*Fase 5 — migração*
26. [ ] `crom-ar migrate` com backup antes e SHA-256 por entrada (cofre real: 187 entradas, 6,9 MB).

*Fase 6 — ecossistema CROM*
27. [ ] `.cromar` com o crompressor (cérebro embutido ou treinado do pacote), roundtrip conferido.
28. [ ] Sincronizar dois cofres só com os blocos que faltam; medir depósito vs. `daemon`/`pull`.
29. [ ] Revisar `brain`, `bench` e `edge`/esqueleto com o depósito: manter, adaptar ou remover.

*Fase 7 — painel e docs*
30. [ ] Painel: disco real, economia por tipo, blocos divididos entre arquivos, progresso de import/GC/migração.
31. [ ] README reescrito sem as regras do V24.

*Fase 8 — testes e aceite*
32. [ ] Unitários do depósito com `-race` (corte, índice, GC, falha no meio, concorrência).
33. [ ] `tests/suite.sh` adaptada; rodar num terminal do sistema (FUSE não monta de dentro do app Claude).
34. [ ] `bench_real.sh` batendo as metas da tabela do PLANO.
35. [ ] Escala: 10 GB variados, 1 arquivo de 5 GB, 100 mil arquivos.
36. [ ] Durabilidade: `kill -9` no import e no GC, depois `doctor` limpo e SHA-256 ok.

**Futuro (só anotado)**
- Vídeo entre arquivos diferentes: só com quadros decodificados (pendência do motor). Hoje o ganho em vídeo vem de cópias e trechos idênticos.
- Atualizar o binário em `../crompressor/bin` (outro repositório: decidir lá).

## ▶️ Retomar

1. Responder a D1 (item 1). Sem ela a fase 1 não começa.
2. Fase 0 primeiro: motor compilado do fonte + `bench_real.sh`, para medir cada passo.
3. Dados de teste: `tests/bench_real.sh` vai baixá-los. Os desta sessão estavam no scratchpad e não ficam.
4. FUSE só testa num terminal do sistema, fora do app Claude (`fusermount3: Operation not permitted`).
5. O residente real está montado em `~/CromAr` (cofre em `~/Documentos/CromAr`): testes sempre com `CROM_AR_ROOT` temporário.
