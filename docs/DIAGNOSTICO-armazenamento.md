# Diagnóstico: o cofre economiza disco? (05/11/2026)

Objetivo do projeto: um "WinRAR" do ecossistema CROM. Jogar dados no cofre e
**ocupar menos disco**, com acesso normal aos arquivos (FUSE, painel, extração).

Resposta curta: **hoje não economiza o que deveria.** Com textos e bancos
diferentes, o cofre fica atrás do `zstd -19`. Com versões de um mesmo dado, que é
onde um cofre deveria brilhar, ele guarda quase tudo de novo.

## Dados reais usados

| Conjunto | Conteúdo | Tamanho |
|---|---|---|
| livros | 30 livros do Project Gutenberg (.txt) | 16,6 MB |
| bancos | Chinook (SQLite + 5 dumps SQL em dialetos), Sakila, Northwind | 34,5 MB |
| versões | 5 `git archive` do crom-tv (40 commits entre cada) + Northwind v1..v4 (poucas linhas alteradas por versão) | 346,8 MB |

## Resultados (bytes em disco, cofre inteiro, incluindo o codebook)

| Conjunto | Cofre, motor V24 antigo | Cofre, motor novo (30/10) | Protótipo por blocos | zstd -19 por arquivo | tar + zstd --long |
|---|---|---|---|---|---|
| livros | 8,96 MB | 6,86 MB | **5,59 MB** | 5,19 MB | 4,63 MB |
| bancos | 12,56 MB | 18,32 MB | **11,29 MB** | 9,99 MB | 9,52 MB |
| versões | **falhou: 9/9 arquivos** | 242,8 MB | **69,5 MB** | 225,3 MB | 65,4 MB |

Protótipo por blocos: CDC (média de 16 KB) + deduplicação de blocos entre arquivos
+ blocos novos agrupados em segmentos de 1 MB comprimidos com zstd (klauspost,
SpeedBestCompression). Roundtrip SHA-256 conferido em todos os arquivos.

## Causas

1. **Motor desatualizado em `bin/`.** O `bin/crompressor` era o V24 de abril.
   Ele corrompe arquivos grandes (≥ 18 MB): a verificação lossless do cofre
   recusou todos. O `make pull-bin` nunca atualizava um binário já existente.
   *Corrigido:* agora o motor vem do `../crompressor` e é recopiado quando muda.
2. **Dedup só por arquivo inteiro (SHA-256).** Duas versões de um banco que
   diferem em 1 linha viram dois `.crom` completos. Não há dedup entre arquivos
   parecidos.
3. **O cérebro do crompressor não serve de depósito de blocos local.**
   - O `train` só guarda blocos **frequentes** e limita os blocos que começam com
     os mesmos 2 bytes. No Northwind ele viu 6.029 blocos e guardou 937, porque
     toda página SQLite começa parecida. A v2 do banco não aproveita a v1 (7,64 MB
     por versão, igual ao zstd).
   - **Correção (mesmo dia):** esse limite já foi corrigido no fonte do motor
     (commit `f88b72f`), mas o binário de `../crompressor/bin` foi compilado antes.
     Compilado do fonte, o `train -k 4096` guarda os 6.029 blocos, e cada versão
     nova vira um `.crom` de ~22 KB. O cérebro, porém, tem 24,7 MB (o banco
     inteiro, sem compressão) e precisa caber na RAM.
   - O cérebro fica em disco sem compressão e tem quase o tamanho do que aprendeu.
     Na rede isso compensa (os dois lados já o têm). No disco local, ele é custo.
   - O cofre treina o cérebro uma única vez, com a primeira pasta importada.
4. **O zstd em Go do motor é mais fraco que o `zstd -19`.** Sem cérebro, o Northwind
   fica 8,9 MB no motor contra 7,8 MB no zstd. O `--max-compression` empata.
5. **Ler pelo FUSE duplica o arquivo no disco.** Abrir um arquivo descomprime ele
   inteiro em `spool/`, e a cópia fica até 24 h. Ler 1 GB custa 1 GB a mais de disco.
6. **`stats` mostra só `vault/`.** O codebook (`brains/`) e o `spool/` não entram
   na conta de "disco", então a economia mostrada é maior que a real.

## O que o motor faz bem (e onde ele deve ficar)

Transferência entre máquinas que já têm o mesmo cérebro: pela rede passa só o que
falta (99,9% de redução em dados já vistos). Isso continua valendo para o pacote
portátil `.cromar` e para sincronizar cofres.

## Proposta

Trocar a camada de armazenamento do cofre por um **depósito de blocos**:

- Cada arquivo vira uma lista de blocos (CDC); um bloco igual é guardado uma vez,
  em qualquer arquivo.
- Blocos novos vão para segmentos de ~1 MB comprimidos com zstd: compressão
  próxima do zstd por arquivo e dedup entre versões.
- O FUSE lê só os segmentos do trecho pedido (cache pequeno em memória), sem
  copiar o arquivo inteiro para o `spool/`.
- Apagar arquivo = contar referências e compactar segmentos órfãos (`doctor --prune`).
- O crompressor continua no `.cromar` e na sincronização, onde o cérebro rende.
- Migração: ler os `.crom` atuais e regravar como blocos, com verificação SHA-256.

## Limitação deste ambiente de teste

O FUSE não monta de dentro do app Claude (o `fusermount3` setuid é bloqueado:
`Operation not permitted`). As 27 falhas da `tests/suite.sh` aqui são todas
disso: o resultado é idêntico com o motor antigo e o novo (66 ✔ / 27 ✘). Os
testes FUSE precisam rodar num terminal normal do sistema. `go test ./...`
passa com o motor novo.
