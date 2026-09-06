# Known Issues — 既知の安全性問題

> 最終検証: 2026-09-06 / 対象 commit `d370c72`
>
> This file is the **live, authoritative status** of the safety issues found in the
> 2026-07-07 safety review. The frozen report with full evidence and reproduction
> scenarios is [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md).
> Update this file (not the report) whenever an issue's status changes.
>
> A second, independent re-audit on 2026-09-06 (against the same commit `d370c72`)
> found 16 additional problems, tracked below as Group R. Its frozen report is
> [docs/raft-audit-2026-09-06.md](docs/raft-audit-2026-09-06.md) — do not edit it;
> record status changes for Group R here, the same as for A–E.

このファイルは [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md)
（2026-07-07 時点の安全性レビュー報告書・凍結）および
[docs/raft-audit-2026-09-06.md](docs/raft-audit-2026-09-06.md)
（2026-09-06 時点の再監査報告書・凍結）で確認された問題の**現在のステータス表**です。
修正が main にマージされたら、該当行の状態と修正 commit をこのファイルで更新してください。
両報告書そのものは編集しないこと。

## サマリ

| 状態 | 件数 |
|---|---|
| ✅ FIXED | 19（A1–A8, B1, B2, C1, C2, C3, C4, D1, D2, D3, D4, D5） |
| 🟠 PARTIAL | 0 |
| ❌ UNFIXED | 19（B3, E1, E2 + グループ R 16 件: R1–R6, R9–R18） |

**実用上の含意**: ログ圧縮（グループ A）の受信側 §7 保持ルール（A7）は解消済みで、圧縮を
有効にしても分岐 suffix を無条件保持することはない。ただし 2026-09-06 の再監査で、圧縮周りに
新たな安全性課題が見つかっている: Raft state と KV snapshot の永続化が別段階で世代整合性がない
（R3）、snapshot のメタデータとペイロードが別世代から読まれうる（R4）、古い snapshot による
KV 状態の後退を防ぐガードがない（R5）。圧縮を有効にする場合はこれらを踏まえて評価すること。
同じ InstallSnapshot 受信経路には B3（`rs.mu` 保持のまま `applyCh` へブロッキング送信）の
**liveness** リスクも残る。

読み取りは ReadIndex 化により線形化されている（D1–D3 解消。選挙直後は当選時 no-op が
コミットされるまで一時的に読みが待たされる）。ただし Log Matching（R1）や AppendEntries の
duplicate ACK（R2）に確認された安全性課題が残るため、「安全性違反なし」とは言えない。

クライアントのリトライは D4/D5 の配線により重複検出の機構自体は実 API 経路に乗ったが、
**ClientID を指定した場合に限る条件付きの at-most-once** であり（`kvstore/store.go:386`）、
「無条件に at-most-once 化済み」ではない。並行リクエストの順序保証や、timeout 発生時に
操作が実際に適用されたかどうかの契約もない（R9, R10）。

なお、これまで暫定策として案内していた `MaxRaftState=0`（圧縮無効）は**設定として指定できない**
（`config/config.go:123-125` の `Validate` が正数を強制する）。A7 修正前の暫定策の記述は
この点で実態と食い違っていた。

## グループ A: ログ圧縮のインデックス体系（すべて通常運転で発火しうる）

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| A1 | AppendEntries 受信側が LastIncludedIndex オフセット未対応 → 圧縮済みフォロワーのログ恒久破壊 | ✅ FIXED | `8ad5367`（絶対index一貫性チェック＋境界を越えない衝突探索） |
| A2 | 投票経路が snapshot を無視し「空ログ」を名乗る → Leader Completeness 違反 | ✅ FIXED | `8ad5367`（RequestVote/startElection を絶対 index で評価、§5.4.1） |
| A3 | 圧縮済みノード当選時に NextIndex が相対/絶対混同 → クラスタ書き込み不能 | ✅ FIXED | `8ad5367`（initializeLeaderState が NextIndex を絶対 index で seed） |
| A4 | updateCommitIndex が相対長と絶対 CommitIndex を比較 → リーダー圧縮後コミット恒久停止 | ✅ FIXED | `8ad5367`（絶対 last index でコミットを前進） |
| A5 | 再起動時に LastApplied がスナップショットから復元されない → 二重適用・範囲外 panic | ✅ FIXED | `8ad5367`（loadPersistentState で CommitIndex/LastApplied を復元＋範囲外ガード） |
| A6 | production で Snapshotter 未配線（3 層のギャップ）→ InstallSnapshot が発火しない/必ず失敗 | ✅ FIXED | `d0cbdc1`（RaftSnapshotter を SetSnapshotter で本番配線＋型アサーションでインターフェース互換を担保）＋ `c516f54`（parseSnapshotBytes で V2 形式をパースし 3 ギャップをすべて解消） |
| A7 | InstallSnapshot 受信側が分岐 suffix を term 検査なしで保持 → Log Matching 違反 | ✅ FIXED | `019d33e`（`logAfterSnapshot` が論文 §7 Figure 13 の受信ルール 6/7 を実装。`LastIncludedIndex` の index/term が一致するときだけ以降を保持し、不一致・照合対象なしなら全破棄） |
| A8 | フォロワー側スナップショットが永続化されない → クラッシュで恒久復元不能 | ✅ FIXED | `c516f54`（installSnapshotFromApplyMsg が saveSnapshot でフォロワー側スナップショットを永続化） |

## グループ B: コミット済みエントリの喪失

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| B1 | applyCh 満杯時にコミット済みコマンドを黙って破棄 | ✅ FIXED | `9a90cf5`（ブロッキング送信化。ただしトレードオフあり — 下記「注記 1」） |
| B2 | AppendEntries の無条件切り詰めでコミット済み suffix が消える（論文 §5.3 step 3 違反） | ✅ FIXED | `7151e77`（論文 §5.3 step 3 準拠の conflict ベース切り詰め） |
| B3 | InstallSnapshot が rs.mu 保持のまま applyCh へブロッキング送信（PLAUSIBLE） | ❌ UNFIXED | `raft/rpc.go:606-607`（`defer rs.mu.Unlock()`）と `raft/rpc.go:668`（保持したままの送信） |

## グループ C: 永続化規律

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| C1 | RequestVote が term/votedFor を persist しない → 同一 term に 2 リーダー | ✅ FIXED | `2a35ce9`（PR #11、応答前 persist + 失敗時 VoteGranted=false） |
| C2 | ハートビート経由の term 更新が persist されない | ✅ FIXED | `2a35ce9`（降格パス 3 箇所も対応） |
| C3 | persist() のエラー無視 | ✅ FIXED | RPC 応答経路は `2a35ce9`。リーダー自身の追記経路は `ffc2926`（`AppendLogEntry` は `(int, error)`、`TruncateLogAfter` は `error` を返し、persist 失敗時はメモリ上の変更をロールバック。`Start()` もエラーを返し、KV 層は即座に操作を失敗させる。当選時 no-op も同様にロールバック） |
| C4 | 永続状態ロード失敗で「記憶喪失ノード」として参加 | ✅ FIXED | `2a35ce9`（ロード失敗時は起動拒否 `main.go:252-255`） |

## グループ D: 読み取り・クライアント処理の linearizability

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| D1 | リース期間にランダム electionTimeout を流用 → stale read | ✅ FIXED | `b3b21a4`（リース機構を撤去。CanServeReadOnlyQuery/lastLeaderConfirmation を削除し ReadIndex に置換） |
| D2 | リース起点が応答受信時刻（送信時刻でなく）→ 違反窓が拡大 | ✅ FIXED | `b3b21a4`（リース撤去によりリース起点そのものが消滅） |
| D3 | 当選時 no-op エントリなし（論文 §8 違反、最も再現容易な stale read） | ✅ FIXED | `60fd631`（becomeLeader が current-term no-op を追加＋ ReadIndex で過半数確認・適用待ち: raft/noop.go, raft/readindex.go） |
| D4 | 重複検出（ClientID/SeqNum）が実 API 経路から未配線 → リトライで二重適用 | ✅ FIXED | `52afd48`（ClientID/SeqNum を PUT/DELETE 経路に配線し at-most-once 化） |
| D5 | 適用成功後に spurious な "leadership lost" エラー → 不要リトライを誘発 | ✅ FIXED | `16a9b31`（コミット済みは log index で解決し、ロール変化後も結果を返却） |

## グループ E: data race

| ID | 概要 | 状態 | 根拠（現コード） |
|---|---|---|---|
| E1 | startElection がロック外で ResetElectionTimer を呼ぶ | ❌ UNFIXED | `raft/rpc.go:248`（persist 失敗パス）と `:262`。`ResetElectionTimer` 自身はロックを取らず `electionTimeout`/`electionTimer`/`lastHeartbeat` を書くため（`raft/state.go:280-287`）、ロック下で呼ぶ `raft/rpc.go:89, 129, 632` と競合する |
| E2 | 送信エントリの backing array を ロック外 marshal 中にハンドラが書き換えうる | ❌ UNFIXED | `raft/rpc.go:403` が `persistent.Log` を再スライスし、`:406` の RUnlock 後 `:425` で marshal する一方、`mergeLogEntries`（`raft/rpc.go:206-230`）が同じ backing array を切り詰め＋追記する |

## グループ R: 2026-09-06 再監査で確認された問題

[docs/raft-audit-2026-09-06.md](docs/raft-audit-2026-09-06.md)（凍結・対象 commit `d370c72`）
で新規に確認された問題。すべて `go test ./...`／`go test -race ./...` は green のままで検出されない
静的確認であり、決定的な障害注入テストはまだ書かれていない。R7・R8 は監査に存在しない（欠番ではなく、
そもそも採番されていない）。

| ID | 概要 | 優先度 | 状態 | 根拠（現コード）/ 監査参照 |
|---|---|---|---|---|
| R1 | `RaftNode.Start` が leader/term 確認と `AppendLogEntry` の durable append を別の `rs.mu` 臨界区間で行うため、その間に降格・高 term 化を挟むと非 leader が command を書き込みうる（Log Matching 違反） | P0 | ❌ UNFIXED | `raft/node.go:99-116`（`Start`）、`raft/log.go:93-111`（`AppendLogEntry`）。監査 §4 P0-1 |
| R2 | AppendEntries が保存失敗後の同一要求の再送を `Success=true` で ACK しうる。`mergeLogEntries` は初回適用時に persist が失敗してもメモリ上の追記をロールバックしないため、再送時は「既に一致」と判定されて persist をスキップし、未永続のまま成功応答が返る。C3 の範囲不足であり、KNOWN 注記 2 の「安全側」評価は誤り | P0 | ❌ UNFIXED | `raft/rpc.go:180-196`（persist スキップ経路）、`:447-450`（`replicateToPeer` が `Success=true` を `MatchIndex` に反映）。監査 §4 P0-2、§2 表「応答前 persist」「Leader Completeness」 |
| R3 | Raft state の境界 persist（InstallSnapshot 受信）と KV snapshot の保存が別段階で行われ、2 ファイル間の世代整合性（atomicity）がない。片方だけ保存できた状態で crash すると復旧不能または不整合になる | P0 | ❌ UNFIXED | `raft/rpc.go:658-665`（Raft 側境界 persist）、`kvstore/store.go:352-357`（KV 側 saveSnapshot）。監査 §4 P0-3、§2 表「snapshot と state の crash atomicity」 |
| R4 | leader 送信側で snapshot のメタデータ（`LastIncludedIndex`/`LastIncludedTerm`）と実データ（`ReadSnapshot()` の戻り値）を別々のタイミングで読むため、両者が別世代になりうる | P0 | ❌ UNFIXED | `raft/rpc.go:379-406`（メタデータ取得、`rs.mu.RLock` 区間）、`:502-512`（`sendSnapshotToPeer` でのデータ読み出し、ロック外）。監査 §4 P0-4、§2 表「snapshot payload/metadata の同一性」 |
| R5 | 受信側は snapshot の新旧を Raft の `LastIncludedIndex` としか比較せず（`volatile.CommitIndex`/`LastApplied` とは無関係）、KV 側 `installSnapshotFromApplyMsg` は適用済みインデックスとの比較なしに無条件で state を置換する。遅延・重複配送された古い snapshot が KV 状態を後退させうる | P0 | ❌ UNFIXED | `raft/rpc.go:635-637`（Raft 側の新旧比較）、`kvstore/store.go:340-344`（KV 側の無条件置換）。監査 §4 P0-5、§2 表「古い snapshot の適用」 |
| R6 | 降格経路（高 term を見た `requestVoteFromPeer`/`replicateToPeer`/`sendSnapshotToPeer`）が `ResetElectionTimer` を呼ばないため、`becomeLeader` で停止した選挙タイマー（`raft/noop.go:35`）が再始動せず、降格後にそのノードが二度と選挙に参加しなくなりうる。E1（ロック外 `ResetElectionTimer`）と隣接する経路だが別の欠陥 | P1 | ❌ UNFIXED | `raft/rpc.go:313-324`（`requestVoteFromPeer`）、`:433-440`（`replicateToPeer`）、`:523-530`（`sendSnapshotToPeer`）、いずれも降格時に `ResetElectionTimer` を呼ばない。監査 §4 P1-1 |
| R9 | `kvstore/store.go:598` で `raft.Start` を呼んだ後、`:611-614` で `pendingOps` に登録するため、その間に committed → applied が完了すると `applyLoop`（`:290-298`）が該当 `opID` を見つけられず結果を握りつぶし、クライアントは実際には成功した操作を timeout として扱う | P1 | ❌ UNFIXED | `kvstore/store.go:598-614`。監査 §4 P1-4。KNOWN D5 の「log index で解決」という記載は現実装（opID ベース）と不一致 |
| R10 | `kvstore/client.go` の `Put`/`Delete` は `seqNum` を採番した後に送信 mutex を解放するため（`:80-91`）、並行呼び出しの到着順を保証しない。`sendRequest`（`:120-184`）は timeout やネットワークエラー時に操作が実際に適用されたかどうかを呼び出し元に伝えない | P1 | ❌ UNFIXED | `kvstore/client.go:80-91,120-184`。監査 §4 P1-5、§3「Go client の内部 retry」 |
| R11 | `Client.Batch`（`kvstore/client.go:216-240`）が `POST /kv/batch` を送るが、`main.go` にそのルートはなく `/kv/` prefix ハンドラ（`handleKV` → `handlePut`）に落ちる。`BatchArgs{Operations}` は `PutArgs{Key,Value}` として空文字列にデコードされ、空 PUT が `success:true` で返る — batch は実装されていないのに黙って（誤った）成功を返す | P1 | ❌ UNFIXED | `kvstore/client.go:216-240`、`main.go:45-70`（`/kv`,`/kv/` のルーティング）。監査 §4 P1-6。TODO.md 8「Advanced Query Features」に batch 計画はあるが、この誤動作は未記載 |
| R12 | `-join` 失敗はログ出力のみで起動は継続する（fail-open）。`ClusterManager` のノード一覧は HTTP レベルの参加/離脱を記録するだけで Raft quorum には反映されない。`StartDiscovery`（`network/discovery.go:99`）は通常起動経路から呼ばれない | P1 | ❌ UNFIXED | `main.go:285-289`、`network/discovery.go:99-116`。監査 §4 P1-7、§3「fixed peers のみ」 |
| R13 | AppendEntries の境界 term 検査・commit 上限が §5.3 の規律を完全にはカバーしない: `PrevLogIndex` が snapshot 境界と一致・それ以下のケースでは term を検証せずに素通りする分岐がある（`:153-163`）。commit index は `min(args.LeaderCommit, rs.lastAbsLogIndex())` で前進するのみで、当該リクエストで実際にマージされた末尾との整合は別途確認されない（`:191-194`） | P1 | ❌ UNFIXED | `raft/rpc.go:153-163,191-194`。監査 §4 P1-8、§2 表「§5.3 prev/commit 規律」 |
| R14 | Joint consensus・構成変更ログエントリ・新旧 quorum の二重確認は未実装。`network/discovery.go` の `ClusterManager` は HTTP レベルの参加/離脱のみで Raft レイヤーの安全なメンバーシップ変更ではない | P2 | ❌ UNFIXED | `raft/state.go:81-84`（peers は固定）。監査 §4 P2「R14」。TODO.md 3「Dynamic Cluster Membership」の計画対象 |
| R15 | InstallSnapshot は `Data []byte` を一括転送するのみで offset/done によるチャンク転送・再送・中断からの再開がない。大容量 snapshot は一括メモリ確保・一括 RPC になる | P2 | ❌ UNFIXED | `raft/rpc.go:39-45`（`InstallSnapshotArgs`）。監査 §4 P2「R15」。`docs/log-compaction.md` Future Enhancements の Streaming 計画に対応 |
| R16 | `config/config.go:23-38` の `SnapshotInterval` は宣言されているが読み出し側で使われていない（自動 snapshot のトリガーは `maxRaftState` のみ）。`LoadConfig`（`:61-83`）はファイルにないフィールドをゼロ値のまま `Validate` に渡すため、`DefaultConfig()` の既定値を経由しない設定ファイルは意図せず起動を拒否されうる | P3 | ❌ UNFIXED | `config/config.go:23-38,61-83`。監査 §4 P3「R16」 |
| R17 | CI (`.github/workflows/ci.yml:37-41`) は `./tests/unit/...` と `./tests/integration/...` のみを `-race` 実行し、`./...`（各パッケージ直下の `_test.go`、例: `raft/installsnapshot_internal_test.go`）を対象にしない。また `tests/integration/cluster_test.go:393` の `break` は `select` から抜けるだけで外側の `for` ループを抜けないため、timeout 後も残りの `done` 受信を待ち続ける（意図した「テスト失敗で即座に打ち切る」動作になっていない） | P3 | ❌ UNFIXED | `.github/workflows/ci.yml:37-41`、`tests/integration/cluster_test.go:393`。監査 §4 P3「R17」 |
| R18 | `examples/benchmark/benchmark.go` の read ワークロードはヒットしない: `populateInitialData`（`:152-162`、鍵生成は `:157`）が `i` を種に `Operations/populateFraction` 件を書き込む一方、読み取り側 `worker`（`:184-222`、鍵生成は `:201`）は `r.Intn(config.Operations/populateFraction)` で毎回ランダムな種を選ぶ。`generateKey` の乱数サフィックスは呼び出しごとに RNG ストリームが進むため、同じ数値シードでも書き込み時と読み取り時で鍵文字列が一致せず、GET はほぼ確実に 404 になる。成功率・引数検証・失敗統計もない | P3 | ❌ UNFIXED | `examples/benchmark/benchmark.go:152-162,184-222,285-287`。監査 §4 P3「R18」 |

## 注記（レビュー後に判明した事実）

1. **B1 修正のトレードオフ**: ブロッキング送信化により、applyCh 逆圧時（例: applyLoop 内の
   同期スナップショット保存中）は rs.mu 保持のまま全 RPC・選挙処理が停止する liveness 問題に
   転化した。専用 applier goroutine への分離が対策（TODO.md の項目 2.5）。
2. **`2a35ce9` の副作用**: AppendEntries でメモリ上のログを切り詰め+追記した後に persist が
   失敗すると `Success=false` を返すが、メモリとディスクの不一致が次回 persist 成功まで残る
   （安全側の挙動）。
   **訂正（2026-09-06 再監査、R2）**: 「安全側」なのは persist が失敗した*その*要求への応答
   （`Success=false`）に限る。同一要求がリトライされた場合、`mergeLogEntries`
   （`raft/rpc.go:206-230`）は前回の（未永続の）メモリ上の追記と比較して「既に一致」と判定して
   `false` を返すため、`raft/rpc.go:180-196` の persist 呼び出し自体がスキップされ、
   `Success=true` が返る。つまり未永続のエントリが再送によって恒久的に「永続化済み」であるかの
   ように扱われる経路があり、単純に安全側とは言えない。詳細は下記グループ R の R2 を参照。
3. **RequestVote の persist 失敗時**: メモリ上の VotedFor は保持したまま `VoteGranted=false`
   のみ返す。同一 term 内の再投票を防ぐ安全方向の意図的設計（`raft/rpc.go:94-104` のコメント参照）。
4. **A6 が CI で検出されなかった理由（解消済み）**: 旧統合テストは `fakeSnapshotter` を使い、
   (i) SetSnapshotter 未配線、(ii) インターフェース非互換、(iii) V2 形式 unmarshal 不整合の
   3 つのギャップをすべて迂回していた。`d0cbdc1`/`c516f54` で 3 ギャップを解消し、
   `tests/integration/snapshot_kvstore_wiring_test.go` が本番配線経路を検証するようになった。
5. **A7 修正で保持する suffix は backing array を共有する**: `logAfterSnapshot` は
   `persistent.Log` を再スライスして返すため（`TruncateLogAfter` と同じ方式）、破棄した
   エントリは配列上に残る。フォロワー受信経路であり E2（リーダー送信経路の race）とは
   別だが、この関数の戻り値をロック外へ持ち出す変更を加える際は注意すること。
6. **InstallSnapshot の persist 失敗時**: ログ置換・スナップショット境界・CommitIndex/
   LastApplied をメモリ上で更新した後に persist が失敗すると、適用は中止するがメモリ側の
   更新は残る（注記 2 と同種の不一致）。A7 修正で新たに生じたものではなく、既存の挙動。

## 修正の推奨順序

2026-07-07 報告書の推奨（B1 → C 群 → B2 → A 群 → D3/D1/D2 → D4/D5）はすべて完了し、
A 群（A1–A8）も A7 の修正で解消した。2026-09-06 再監査（グループ R）を踏まえた現在の推奨順は
`docs/raft-audit-2026-09-06.md` §6 のロードマップに従う:

1. 現状訂正のみ（文書・行番号・保証範囲） — 本 PR
2. CI を `go test -race ./...` に拡大し、`tests/integration/cluster_test.go:393` の
   timeout `break` を修正（R17）
3. R1（`Start` の atomic 化）、R2（durable ACK）
4. R3–R5（snapshot の世代整合・復旧・適用順序）
5. R6、E1/E2（peer replication worker とタイマー/リーダー参照の統一）
6. B3（ordered applier と shutdown lifecycle）
7. R9–R13（KV/client/API/RPC 細部）
8. R15（chunk transfer）
9. R14（joint consensus: state → quorum → 管理 API → snapshot の順）
10. R16–R18（設定、CI 監視、examples、benchmark）
