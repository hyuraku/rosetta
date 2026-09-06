# rosetta Raft 再監査（2026-09-06）

> Last verified: 2026-09-06 against commit `d370c72`. Frozen point-in-time report — do not edit; record status changes in KNOWN_ISSUES.md.

## 1. 監査範囲と結論

対象は Git HEAD `d370c72361e367ec4a5516133cea419a1afba9bd`。`git status --short` では未追跡の `prompts/` のみで、追跡済みのコード・テスト・文書に変更はない。未追跡ファイルは実装根拠に使用していない。

一次資料は Diego Ongaro / John Ousterhout, *In Search of an Understandable Consensus Algorithm (Extended Version)*（Raft extended paper、2014-05-20、18 pages、[raft.pdf](https://raft.github.io/raft.pdf)）。ReadIndex の具体的手順は別資料である Ongaro, *Consensus: Bridging Theory and Practice*（2014-08、§6.4、[dissertation](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)）と照合した。

検証結果：`go test ./...`、`go test -race ./...`、`go test -count=1 ./tests/unit` は成功した。ただし、これらは新規障害注入を含まない。以下の新規問題はコードから成立条件を確認した「静的確認」である。

利用者が現在できることは、固定 peer 構成での leader election、AppendEntries による複製、HTTP PUT/GET/DELETE、leader の ReadIndex 読み取り、term/vote/log と snapshot の通常保存・復元、ClientID/SeqNum を指定した重複検出である。フォロワーは読み書きを 503 で拒否する。

動的 membership / joint consensus、実用的な `-join`、安全なオンライン backup、分割 snapshot 転送、認証・TLS・metrics、任意障害下での安全性保証は提供されない。さらに、Start と降格の競合、保存失敗後の再送 ACK、snapshot の世代・復旧整合性に P0 相当の問題があるため、production-ready と表現できない。

## 2. Raft 論文との機能対応

### 安全性

| 論文要件 | 判定 | コード根拠 | テスト／未検証 | 文書状況 |
|---|---|---|---|---|
| Figure 2 の persistent/volatile/leader state | 実装済み | `raft/state.go:52-70` | `TestRaftStateInitialization`、再起動テスト | `docs/raft-paper-implementation-status.md:165` は概ね正しい |
| RequestVote / AppendEntries | 実装済み | `raft/rpc.go:57`, `:110`、`main.go:261-274` | `TestRequestVote`、`TestAppendEntries` | 対照表 §5.2/§5.3 |
| 応答前 persist | 要修正 | AppendEntries は `raft/rpc.go:180-196`。保存失敗後の同一要求は再保存せず成功しうる（R2） | 失敗→同一再送→crash は未検証 | KNOWN C3 `KNOWN_ISSUES.md:62` の「安全側」は誤り |
| §5.2 election | 部分実装 | timeout・自己投票・quorum は `raft/rpc.go:232-281` | election 基本試験は成功。高 term 後の停止は未検証 | 対照表 `:28` は基本動作のみ正しい |
| §5.3 conflict truncation | 実装済み | `raft/rpc.go:206-229` | stale/conflict テスト成功 | KNOWN B2 `:53` は正しい |
| §5.3 prev/commit 規律 | 要修正 | 境界 term を検査しない `raft/rpc.go:153-163`、commit 上限 `:191-194`（R13） | 境界不一致・短い要求は未検証 | A1 の絶対 index 修正とは別 |
| §5.4 Election Safety | 実装済み（限定） | vote の term/index 比較 `raft/rpc.go:78-90` | vote/election tests 成功 | C1/A2 の修正は現コードに対応 |
| §5.4 Log Matching | 要修正 | Start が leader 確認後に別ロックで追記し、降格後も追記可能 `raft/node.go:99-115`, `raft/log.go:93-108`（R1） | 強制 interleave 未実行 | 「既知の違反なし」`docs/raft-paper-implementation-status.md:133` は矛盾 |
| §5.4 Leader Completeness | 要修正 | 非永続 follower の duplicate ACK が leader の `MatchIndex` に数えられる（R2、`raft/rpc.go:447-450`） | crash/re-election 未検証 | KNOWN C3 の範囲不足 |
| §5.4 State Machine Safety | 要修正 | R1 の同 index/term 別 command、R3-R5 の snapshot 不整合 | 専用履歴検証なし | KNOWN と textbook の総括が過大 |
| §6 joint consensus | 未実装 | peers は固定 `raft/state.go:81-84`。構成エントリ・二重 quorum なし | membership 試験なし | TODO `:153`、対照表 `:262` は正しい |
| §7 snapshot suffix 保持 | 部分実装 | A7 の `logAfterSnapshot` は `raft/rpc.go:639-645` で使用 | A7 unit tests 成功 | KNOWN A7 `:45` は正しい |
| snapshot と state の crash atomicity | 要修正 | Raft 境界保存 `raft/rpc.go:658-665` と KV 保存 `kvstore/store.go:352-357` が別段階（R3） | 保存途中クラッシュ未検証 | A8 `KNOWN_ISSUES.md:46` は呼び出し追加のみを記載 |
| snapshot payload/metadata の同一性 | 要修正 | 境界取得 `raft/rpc.go:379-406` と Data 読み出し `:502-512` が別世代（R4） | 世代競合未検証 | 未記載 |
| 古い snapshot の適用 | 要修正 | Raft は境界だけ比較 `raft/rpc.go:635-637`、KV は無条件置換 `kvstore/store.go:340-344`（R5） | 遅延 snapshot 未検証 | 未記載 |

### Liveness・クライアント・追加要件

| 要件 | 判定 | コード根拠 | テスト／文書 |
|---|---|---|---|
| leader timer / 高 term 処理 | 要修正 | leader で timer 停止 `raft/noop.go:30-36`。拒否経路や降格経路で再開しない（R6） | 典型 election は成功。R6 は未検証。対照表の E1 のみという説明は不足 |
| apply の liveness | 要修正 | `applyCh` への送信を `rs.mu` 保持中に行う `raft/rpc.go:668` 等（既知 B3） | race は成功でも遅い consumer は未検証。KNOWN B3/ TODO 2.5 は正しい |
| ReadIndex | 実装済み（下層安全性に依存） | `raft/readindex.go:51-90`、`kvstore/store.go:474-504` | `TestReadIndexConfirmsWithQuorum` 等成功 | CLAUDE `:27`、対照表 `:351` は機構を正しく説明。具体的方式は dissertation §6.4 |
| §8 retry/dedup | 部分実装 | `kvstore/store.go:384-425`、HTTP 配線 `main.go:86`, `:148` | session tests 成功。並行到着・呼び直し未検証 | D4 配線済みは正しいが「無条件 at-most-once」は過大 |
| 結果待ち | 要修正 | Start `kvstore/store.go:598` の後で pending 登録 `:611-614`。早期適用結果を捨てうる（R9） | 未検証 | 未記載。D5 の「log index」は現実装と不一致 |
| batch API | 要修正 | client `kvstore/client.go:216-240` の `/kv/batch` が通常 `/kv` handler に入り空 PUT になりうる（R11） | 実 HTTP batch 未検証 | TODO 8 に計画、ライブ台帳には未記載 |
| discovery / join | 部分実装（実用経路なし） | `StartDiscovery` 定義 `network/discovery.go:99`、通常起動は `main.go:280-289` で失敗をログのみ（R12） | 未検証 | textbook `:758-775` は歴史的に正しい。README の利用可能表現は矛盾 |
| snapshot chunking | 未実装 | `InstallSnapshotArgs` は全量 Data、offset/done なし `raft/rpc.go:39-45` | 大容量・中断未検証 | log-compaction の Future Enhancements に記載 |

## 3. 現在のプロダクト機能と制約

- HTTP は `main.go:45-56` の `/kv`, `/status`, `/leader`。PUT/POST は body の key/value、GET は key path、DELETE は optional body の session。フォロワーは 503 と `X-Raft-Leader`。
- Read は ReadIndex→適用待ち→local state の順。選挙直後の current-term no-op 未 commit 時は失敗する。
- `status.log_size` は保持エントリ数ではなく、`raft/node.go:142-144` の絶対 last log index。
- FileStorage は file 単位では atomic write-rename (`persistence/file_storage.go:31-56`)。しかし Raft state と KV snapshot の世代横断 atomicity はない。
- 自動 snapshot は `maxRaftState` 件数を適用 loop で数える。`SnapshotInterval` は通常経路で使われない。no-op は通常 command count に加算されない。
- fixed peers のみ。ClusterManager のノード一覧を Raft quorum に反映するコードはない。
- 認証、TLS、rate limit、metrics、health、分割 snapshot 転送はない。
- Go client の内部 retry は同じ request body を再利用するが、呼び出しをやり直すと新しい seq。並行 Put は seq 採番後に送信 mutex を解放するため、サーバー到着順の契約はない。

## 4. 直すべきコード・追加コード

### P0

1. **R1（未記載）Start の leader 判定と append を原子化**：`raft/node.go:99-108` と `raft/log.go:93-105`。同一 `rs.mu` で role/term 確認から durable append まで行う。高 term 降格を挟む interleave test を追加し、非 leader が command を生成しないことを完了条件とする。
2. **R2（C3 の範囲不足）AppendEntries の保存失敗後に ACK しない**：`raft/rpc.go:180-196`。未永続状態を fail-stop または候補状態へ隔離し、duplicate path でも durable 状態を確認する。失敗→同一再送→leader commit→crash→再選挙テストを追加。
3. **R3（未記載）Raft state と KV snapshot の整合世代**：`raft/rpc.go:644-668`、`kvstore/store.go:340-357`、`persistence/`。payload・index・term・Raft 境界を一つの世代として atomic publish し、起動時に不整合なら fail closed。各保存段階の crash/error test を追加。
4. **R4（未記載）snapshot metadata と payload の同一世代化**：`raft/rpc.go:379-406,502-512`、`persistence/raft_snapshotter.go`。immutable `(index,term,data)` envelope を返す。世代競合テストを追加。
5. **R5（未記載）古い snapshot による KV 後退を禁止**：`raft/rpc.go:635-637`、`kvstore/store.go:340-344`。applied/commit index より古い snapshot を無視し、KV 側にも単調性ガード。遅延 snapshot→後続 command test を追加。

### P1

1. **R6（未記載）降格時の timer と高 term 応答を統一**：`raft/noop.go:35`、`raft/rpc.go:313-324,433-440,523-530`。共通 follower transition を設け、再選挙・storage 回復テストを追加。
2. **E1/E2＋追加 race（E1/E2 は KNOWN `:79-80`）**：`raft/rpc.go:248,262,403,406`、`raft/node.go:126-135`。timer・currentLeader の mutex 統一、送信 slice copy、peer ごとの単一 worker／generation guard。逆順 ACK、marshal 競合、race test。
3. **B3 と shutdown（B3 は KNOWN `:54`、TODO 2.5）**：`raft/rpc.go:668`、`raft/node.go:138-140`、`main.go:309-312`。ordered applier、context/WaitGroup、HTTP→transport→Raft→KV の停止順。遅い consumer と負荷中 shutdown test。
4. **R9（未記載）pending 登録を Start より前へ**：`kvstore/store.go:598-614`。早期 apply で結果を失わない test、timeout cleanup。
5. **R10（未記載）client session の並行・結果不明契約**：`kvstore/client.go:80-91,120-184`。最小案は client ごと一要求、または retry token と durable client identity。実 HTTP で順序逆転・応答喪失 test。
6. **R11（未記載）batch を未実装として拒否**：`kvstore/client.go:216-240`、`main.go:45-70`。batch が空 PUT として成功しない handler test。
7. **R12（未記載）join 失敗時の fail-closed**：`main.go:264-289`、`network/discovery.go:99-116`。membership 実装までは join 指定で起動拒否。自己 peer／無効構成 test。
8. **R13（未記載）AppendEntries 境界・commit 上限**：`raft/rpc.go:153-163,191-194`。boundary term と当該 request の照合末尾を検査。短い request／不一致 test。

### P2

- **R14** joint consensus、構成ログ、snapshot 内構成、旧・新 quorum を `raft/`・`persistence/`・管理 API に実装。構成変更中の障害と再起動を試験。
- **R15** immutable snapshot を前提に chunk/offset/done、再送、途中状態の非公開を実装。大容量・中断試験。

### P3

- **R16** 未使用設定（`config/config.go:23-38`）を実際に配線するか拒否・予約扱いにする。`LoadConfig:73` の default 補完も明確化。
- **R17** CI (`.github/workflows/ci.yml:37-41`) を `go test -race ./...` に広げる。`tests/integration/cluster_test.go:393` の timeout break を修正。
- **R18** status、examples、外部 benchmark の契約を修正。`examples/benchmark/benchmark.go:157-160,201-206` は preload と GET のキー生成が一致せず hit workload にならない。成功率・引数検証・失敗統計を追加。

## 5. ドキュメント監査

### 正しい記載

KNOWN の A1–A8、B2、E1/E2、B3 の個別説明（`KNOWN_ISSUES.md:35-80`）、CLAUDE の ReadIndex と follower write 制約（`CLAUDE.md:16-29`）、固定 membership が未実装という TODO／論文対照表は、対応する現コードと整合する。

### 古い・矛盾する記載

- `KNOWN_ISSUES.md:23-29,106-109`、`docs/raft-paper-implementation-status.md:11-22`、`docs/log-compaction.md:379`、`docs/textbook.md:1042` は「安全性違反なし」「残りは B3/E1/E2」と断定する。R1–R5 を反映する。
- `KNOWN_ISSUES.md:87-89` は AppendEntries 保存失敗後の不一致を「安全側」と評価する。R2 の duplicate ACK を追記する。
- `KNOWN_ISSUES.md:63`、`docs/raft-paper-implementation-status.md:194-197` は行番号が古い、または RequestVote の memory vote を「取り消す」と誤記する。
- `docs/persistence.md:223-237,334-336,375-379` は A5/A6/A8 の古い未修正警告。`raft/state.go:182-183`、`main.go:273`、`store.go:354` に合わせて置換する。
- `docs/api.md:255`、`:277-298` は D4/D5 未修正と説明する。現在は配線済みだが、条件付き dedup と結果不明を記載する。`:179` の log_size 説明も絶対 last index へ修正する。
- `README.md` の discovery/join・durability・linearizability の表現は、固定構成・条件付き保証へ狭める。
- `examples/simple-cluster/README.md`、`start.sh`、`demo.sh` は実際に存在しない `.state`、`.leader_id`、`.log_length` や follower GET 成功を前提にしている。
- `CLAUDE.md:15,28-30` は benchmark flag、FileStorage の型、snapshot hash・pending ID の説明が現コードと一致しない。

### 記載漏れ

R1–R5、R6、R9–R13、snapshot 世代整合、join fail-open、CI の package-local test 除外、benchmark の測定不備はライブ台帳にない。追加時は既存 A/C/D の修正済み履歴を覆さず、新しい ID として記録する。

### 更新順

文書専用 PR で KNOWN、論文対照表、README/CLAUDE、persistence/log-compaction/API、TODO、examples の順に現状を訂正する。`docs/safety-review-2026-07-07.md` は凍結資料なので編集しない。動作変更 PR は CLAUDE の same-PR rule に従い、実装した仕様だけを同時更新する。

## 6. 実装ロードマップ

1. 現状訂正のみ（文書・行番号・保証範囲）。
2. CI を `./...` に拡大し、timeout 終了条件を修正。
3. R1 の atomic Start、R2 の durable ACK。
4. R3–R5 の snapshot 世代・復旧・適用順序。
5. R6、E1/E2、peer replication worker。
6. B3 の ordered applier と shutdown lifecycle。
7. R9–R13 の KV/client/API/RPC 細部。
8. R15 の chunk transfer。
9. R14 の joint consensus（state→quorum→管理 API→snapshot の順）。
10. R16–R18 の設定、監視、examples、benchmark。

PR1 は現状の正しい文書化だけにし、機能・安全性を変える PR と混ぜない。以後の動作変更は影響文書と KNOWN を同一 PR で更新する。

## 7. 調査上の限界

実 HTTP 複数プロセス、電源断相当の crash、disk full/fsync/rename 失敗、snapshot 保存途中の再起動、RPC 遅延・逆順・重複、大容量 snapshot、長時間 stress、履歴 checker、fuzzing、examples 実行、benchmark 測定は実施していない。したがって、既存テストの成功は確認済みだが、新規 P0/P1 は静的確認であり、上記ロードマップの決定的障害テストで再確認する必要がある。
