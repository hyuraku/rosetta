# Known Issues — 既知の安全性問題

> 最終検証: 2026-09-06 against commit `980f43d`
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
| ✅ FIXED | 30（A1–A8, B1, B2, B3, C1, C2, C3, C4, D1, D2, D3, D4, D5, E1, E2, R1, R2, R3, R4, R5, R6, R17, R19） |
| 🟠 PARTIAL | 0 |
| ❌ UNFIXED | 9（グループ R: R9–R16, R18） |

**実用上の含意**: ログ圧縮（グループ A）の受信側 §7 保持ルール（A7）は解消済みで、圧縮を
有効にしても分岐 suffix を無条件保持することはない。2026-09-06 の再監査で見つかった snapshot
経路の 3 件（R3: Raft state と KV snapshot の世代整合、R4: メタデータとペイロードの同一世代化、
R5: 古い snapshot による後退の禁止）も解消した（`0695b95` / `f53617e` / `156510a`）。
InstallSnapshot 受信は「KV payload を durable 化 → Raft 境界を durable 化 → メモリ適用」の
順序不変条件を守り、crash 時は必ず復旧可能な側（snapshot が Raft より新しい）に倒れる。
同じ受信経路に残っていた B3（`rs.mu` 保持のまま `applyCh` へブロッキング送信）の **liveness**
リスクも解消した（`f873d9b`）: 適用は専用の applier goroutine に分離され、コミット経路と
InstallSnapshot 受信は「適用可能になった」ことを通知するだけになったため、遅い state machine や
遅い storage が RPC と選挙タイマーを止めることはなくなった。順序不変条件（payload → 境界 →
メモリ）は受信ハンドラ内でそのまま保たれ、非同期になったのは 3 番目の引き渡しだけである。

停止処理も同時に整理した（R19・`7c96f14` / `13570d5`）。`RaftNode.Kill` は自分が起動した
goroutine（event loop・applier・replication・投票・ReadIndex heartbeat・圧縮）の終了を待って
から返るようになり、`main.go` は「HTTP API → Raft transport → `raftNode.Kill()` → `kvs.Close()`」
の順で停止する。applyCh の送信者は applier ただ 1 本で、Kill が返った時点で終了しているため、
その後の `Close()` で `send on closed channel` に落ちることはない。

読み取りは ReadIndex 化により線形化されている（D1–D3 解消。選挙直後は当選時 no-op が
コミットされるまで一時的に読みが待たされる）。Log Matching（R1: `Start` の leader 確認と
append の原子化）と AppendEntries の duplicate ACK（R2: 未永続 merge のロールバック）は解消した
（`2c26b9a` / `c362ae4`）。R3–R5 の解消により、監査が指摘した snapshot 経路の State Machine
Safety 違反経路は塞がった。ただし R9–R13 をはじめ未修正の課題が残るため、
「安全性違反なし」とは言えない。

降格処理は `becomeFollowerLocked` に一本化され（R6・`ac93fcb`）、高 term を見て降格した元 leader も
選挙タイマーが再始動して自力で立候補できる。既知の data race 2 件も解消した（E1・`c5fdc0f`、
E2・`7e3eb61`）。あわせて replication は peer ごとに直列化され、`MatchIndex`/`NextIndex` は
成功応答で後退しなくなった（`499c4b8`）— 同一 follower に AppendEntries と 5 秒の
InstallSnapshot が並走し、応答が逆順に戻ると follower の進捗が巻き戻る問題への対処。

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
| B3 | InstallSnapshot（および全コミット経路）が rs.mu 保持のまま applyCh へブロッキング送信（PLAUSIBLE）。R3 の修正で、同じ臨界区間に snapshot payload の同期書き込みも加わった | ✅ FIXED | `f873d9b`（専用 applier goroutine に分離: `raft/applier.go`。`UpdateCommitIndex`／`AppendEntries`／leader の `updateCommitIndex`／`InstallSnapshot` は `rs.mu` 下で `notifyApplierLocked` を呼ぶだけになり、applier が `takeApplyWork` でバッチを**コピー**してからロック外で送信する。`LastApplied` はバッチ確保時にロック内で進める — R5 の単調性ガードが同じロックで `LastApplied` を見るため、snapshot は「applier がこれから渡す全 command より厳密に新しい」ときだけ受理され、これが 2 系統の順序を決める。snapshot index N より上の command は必ず snapshot の後に届く。N 以下の command は先行バッチの残りとして後に届きうるが、KV 側の単調性ガードが捨てる。InstallSnapshot の順序不変条件（payload durable → 境界 durable → メモリ適用）はハンドラ内でそのまま。回帰テスト `raft/applier_internal_test.go`） |

## グループ C: 永続化規律

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| C1 | RequestVote が term/votedFor を persist しない → 同一 term に 2 リーダー | ✅ FIXED | `2a35ce9`（PR #11、応答前 persist + 失敗時 VoteGranted=false） |
| C2 | ハートビート経由の term 更新が persist されない | ✅ FIXED | `2a35ce9`（降格パス 3 箇所も対応） |
| C3 | persist() のエラー無視 | ✅ FIXED | RPC 応答経路は `2a35ce9`。リーダー自身の追記経路は `ffc2926`（`AppendLogEntry` は `(int, error)`、`TruncateLogAfter` は `error` を返し、persist 失敗時はメモリ上の変更をロールバック。`Start()` もエラーを返し、KV 層は即座に操作を失敗させる。当選時 no-op も同様にロールバック） |
| C4 | 永続状態ロード失敗で「記憶喪失ノード」として参加 | ✅ FIXED | `2a35ce9`（ロード失敗時は起動拒否 `main.go:294-297`） |

## グループ D: 読み取り・クライアント処理の linearizability

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| D1 | リース期間にランダム electionTimeout を流用 → stale read | ✅ FIXED | `b3b21a4`（リース機構を撤去。CanServeReadOnlyQuery/lastLeaderConfirmation を削除し ReadIndex に置換） |
| D2 | リース起点が応答受信時刻（送信時刻でなく）→ 違反窓が拡大 | ✅ FIXED | `b3b21a4`（リース撤去によりリース起点そのものが消滅） |
| D3 | 当選時 no-op エントリなし（論文 §8 違反、最も再現容易な stale read） | ✅ FIXED | `60fd631`（becomeLeader が current-term no-op を追加＋ ReadIndex で過半数確認・適用待ち: raft/noop.go, raft/readindex.go） |
| D4 | 重複検出（ClientID/SeqNum）が実 API 経路から未配線 → リトライで二重適用 | ✅ FIXED | `52afd48`（ClientID/SeqNum を PUT/DELETE 経路に配線し at-most-once 化） |
| D5 | 適用成功後に spurious な "leadership lost" エラー → 不要リトライを誘発 | ✅ FIXED | `16a9b31`（コミット済みは log index で解決し、ロール変化後も結果を返却） |

## グループ E: data race

| ID | 概要 | 状態 | 根拠（現コード）/ 修正 commit |
|---|---|---|---|
| E1 | startElection がロック外で ResetElectionTimer を呼ぶ | ✅ FIXED | `c5fdc0f`（`resetElectionTimerLocked` を新設し `rs.mu` 保持下の内部経路をすべてそれに統一。公開 `ResetElectionTimer` は自分でロックを取るので外部呼び出し元は不変。`startElection` は term を刻むのと同じ臨界区間で再始動する。`RaftNode.GetLeader` が `rs.mu` なしで読んでいた `currentLeader` も `GetCurrentLeader()` 経由に変更。回帰テスト `raft/timerrace_internal_test.go`） |
| E2 | 送信エントリの backing array を ロック外 marshal 中にハンドラが書き換えうる | ✅ FIXED | `7e3eb61`（`replicateToPeer` がロック内で entries を新しいスライスへコピーしてから transport に渡す。`LogEntry.Command` は受信側が読むだけなので浅いコピーで足りる。1 回の送信量の上限（maxEntriesPerAppend）は未導入で範囲外。回帰テスト `raft/sendentries_internal_test.go`） |

## グループ R: 2026-09-06 再監査で確認された問題

[docs/raft-audit-2026-09-06.md](docs/raft-audit-2026-09-06.md)（凍結・対象 commit `d370c72`）
で新規に確認された問題（R1–R18）と、その修正作業中に見つかった問題（R19 以降。監査には存在しない）。
R1–R18 はいずれも `go test ./...`／`go test -race ./...` が green のままで検出されない静的確認
として起票された。修正済みの R1–R5・R17・R19 には障害注入・世代競合・停止順の決定的テストが
付いているが、未修正の項目にはまだない。R19 は例外的に `-race` でも捕まる panic だったが、
発火が停止時のタイミング依存だったため CI では不安定失敗としてしか現れていなかった。
R7・R8 は監査に存在しない（欠番ではなく、そもそも採番されていない）。

| ID | 概要 | 優先度 | 状態 | 根拠（現コード）/ 監査参照 |
|---|---|---|---|---|
| R1 | `RaftNode.Start` が leader/term 確認と `AppendLogEntry` の durable append を別の `rs.mu` 臨界区間で行うため、その間に降格・高 term 化を挟むと非 leader が command を書き込みうる（Log Matching 違反） | P0 | ✅ FIXED | `2c26b9a`（`RaftState.Start` が role 確認・term stamp・persist・ロールバックを `rs.mu` 1 回の中で行う。追記本体は `appendEntryLocked` に切り出し `appendNoOpLocked` と共有。`RaftNode.Start` は委譲のみで返り値の契約は不変） |
| R2 | AppendEntries が保存失敗後の同一要求の再送を `Success=true` で ACK しうる。`mergeLogEntries` は初回適用時に persist が失敗してもメモリ上の追記をロールバックしないため、再送時は「既に一致」と判定されて persist をスキップし、未永続のまま成功応答が返る。C3 の範囲不足であり、KNOWN 注記 2 の「安全側」評価は誤り | P0 | ✅ FIXED | `c362ae4`（persist 失敗時に merge 前のログへロールバック。`mergeLogEntries` は衝突位置で `Log[:pos:pos]` と cap を切って追記するため上書きが起きず、保存しておいた slice header が有効な復元先になる。dirty フラグ方式ではなく「呼び出し完了後はメモリとディスクが一致する」という単一不変条件を選択） |
| R3 | Raft state の境界 persist（InstallSnapshot 受信）と KV snapshot の保存が別段階で行われ、2 ファイル間の世代整合性（atomicity）がない。片方だけ保存できた状態で crash すると復旧不能または不整合になる | P0 | ✅ FIXED | `0695b95`（受信経路に「KV payload durable → Raft 境界 durable → メモリ適用」の順序不変条件を導入し、handler の doc comment に明記。`raft/rpc.go:678-789`。crash は必ず「snapshot が Raft より新しい」復旧可能な側に倒れる。`ApplyMsg.SnapshotPersisted` で KV 側の二重書きを抑止。起動時は `persistence.VerifySnapshotConsistency`（`persistence/consistency.go:52`）を `main.go:240` の `verifyDurableState` から呼び、Raft 境界が snapshot より進んでいれば起動拒否（C4 と同じ fail-closed）、逆向きは applyLoop の単調性ガード（`kvstore/store.go:265-268`）が吸収。`InstallSnapshot`/`TruncateLogTo` は persist 失敗時にメモリをロールバック） |
| R4 | leader 送信側で snapshot のメタデータ（`LastIncludedIndex`/`LastIncludedTerm`）と実データ（`ReadSnapshot()` の戻り値）を別々のタイミングで読むため、両者が別世代になりうる | P0 | ✅ FIXED | `f53617e`（`Snapshotter.ReadSnapshot` が `*SnapshotData`（index/term/data）の immutable envelope を返すよう変更。`sendSnapshotToPeer`（`raft/rpc.go:533-590`）は `InstallSnapshotArgs` と follower の MatchIndex/NextIndex をその envelope だけから組み立てる。ロック内で読んだ nextIndex は「その envelope が follower を前進させられるか」の判定にのみ使う） |
| R5 | 受信側は snapshot の新旧を Raft の `LastIncludedIndex` としか比較せず（`volatile.CommitIndex`/`LastApplied` とは無関係）、KV 側 `installSnapshotFromApplyMsg` は適用済みインデックスとの比較なしに無条件で state を置換する。遅延・重複配送された古い snapshot が KV 状態を後退させうる | P0 | ✅ FIXED | `156510a`（Raft 側は `args.LastIncludedIndex <= volatile.LastApplied` なら term 更新と timer reset だけ行って無視する: `raft/rpc.go:716-722`。`InstallSnapshotFromData` にも同じ規則。KV 側 `installSnapshotFromApplyMsg` は `msg.SnapshotIndex <= lastAppliedIndex` を無視する単調性ガードを持つ: `kvstore/store.go:355-362`） |
| R6 | 降格経路（高 term を見た `requestVoteFromPeer`/`replicateToPeer`/`sendSnapshotToPeer`）が `ResetElectionTimer` を呼ばないため、`becomeLeader` で停止した選挙タイマー（`raft/noop.go:35`）が再始動せず、降格後にそのノードが二度と選挙に参加しなくなりうる。E1（ロック外 `ResetElectionTimer`）と隣接する経路だが別の欠陥 | P1 | ✅ FIXED | `ac93fcb`（`becomeFollowerLocked`（`raft/state.go`）を唯一の follower 遷移として新設。state=Follower・term 更新と VotedFor=nil・`currentLeader`・`rs.leader = nil`・選挙タイマー再始動・persist を 1 箇所にまとめ、降格 3 経路と `stepDown`（`raft/readindex.go`）、受信 3 経路（RequestVote/AppendEntries/InstallSnapshot）、`startElection` の persist 失敗経路をすべてこれに統一。persist は term が動いたときだけ行うので heartbeat 経路は従来どおりディスク I/O なし。persist 失敗時の方針は各経路の従来どおり（RPC 応答経路は拒否応答、送信側は log のみ）。`rs.leader = nil` により「`rs.leader != nil` ⇔ leader」が成り立つので `replicateToPeer` は読み出し前に role を再確認する。回帰テスト `raft/followertransition_internal_test.go`） |
| R9 | `kvstore/store.go:598` で `raft.Start` を呼んだ後、`:611-614` で `pendingOps` に登録するため、その間に committed → applied が完了すると `applyLoop`（`:290-298`）が該当 `opID` を見つけられず結果を握りつぶし、クライアントは実際には成功した操作を timeout として扱う | P1 | ❌ UNFIXED | `kvstore/store.go:598-614`。監査 §4 P1-4。KNOWN D5 の「log index で解決」という記載は現実装（opID ベース）と不一致 |
| R10 | `kvstore/client.go` の `Put`/`Delete` は `seqNum` を採番した後に送信 mutex を解放するため（`:80-91`）、並行呼び出しの到着順を保証しない。`sendRequest`（`:120-184`）は timeout やネットワークエラー時に操作が実際に適用されたかどうかを呼び出し元に伝えない | P1 | ❌ UNFIXED | `kvstore/client.go:80-91,120-184`。監査 §4 P1-5、§3「Go client の内部 retry」 |
| R11 | `Client.Batch`（`kvstore/client.go:216-240`）が `POST /kv/batch` を送るが、`main.go` にそのルートはなく `/kv/` prefix ハンドラ（`handleKV` → `handlePut`）に落ちる。`BatchArgs{Operations}` は `PutArgs{Key,Value}` として空文字列にデコードされ、空 PUT が `success:true` で返る — batch は実装されていないのに黙って（誤った）成功を返す | P1 | ❌ UNFIXED | `kvstore/client.go:216-240`、`main.go:45-70`（`/kv`,`/kv/` のルーティング）。監査 §4 P1-6。TODO.md 8「Advanced Query Features」に batch 計画はあるが、この誤動作は未記載 |
| R12 | `-join` 失敗はログ出力のみで起動は継続する（fail-open）。`ClusterManager` のノード一覧は HTTP レベルの参加/離脱を記録するだけで Raft quorum には反映されない。`StartDiscovery`（`network/discovery.go:99`）は通常起動経路から呼ばれない | P1 | ❌ UNFIXED | `main.go:285-289`、`network/discovery.go:99-116`。監査 §4 P1-7、§3「fixed peers のみ」 |
| R13 | AppendEntries の境界 term 検査・commit 上限が §5.3 の規律を完全にはカバーしない: `PrevLogIndex` が snapshot 境界と一致・それ以下のケースでは term を検証せずに素通りする分岐がある（`:153-163`）。commit index は `min(args.LeaderCommit, rs.lastAbsLogIndex())` で前進するのみで、当該リクエストで実際にマージされた末尾との整合は別途確認されない（`:191-194`） | P1 | ❌ UNFIXED | `raft/rpc.go:153-163,191-194`。監査 §4 P1-8、§2 表「§5.3 prev/commit 規律」 |
| R14 | Joint consensus・構成変更ログエントリ・新旧 quorum の二重確認は未実装。`network/discovery.go` の `ClusterManager` は HTTP レベルの参加/離脱のみで Raft レイヤーの安全なメンバーシップ変更ではない | P2 | ❌ UNFIXED | `raft/state.go:81-84`（peers は固定）。監査 §4 P2「R14」。TODO.md 3「Dynamic Cluster Membership」の計画対象 |
| R15 | InstallSnapshot は `Data []byte` を一括転送するのみで offset/done によるチャンク転送・再送・中断からの再開がない。大容量 snapshot は一括メモリ確保・一括 RPC になる | P2 | ❌ UNFIXED | `raft/rpc.go:39-45`（`InstallSnapshotArgs`）。監査 §4 P2「R15」。`docs/log-compaction.md` Future Enhancements の Streaming 計画に対応 |
| R16 | `config/config.go:23-38` の `SnapshotInterval` は宣言されているが読み出し側で使われていない（自動 snapshot のトリガーは `maxRaftState` のみ）。`LoadConfig`（`:61-83`）はファイルにないフィールドをゼロ値のまま `Validate` に渡すため、`DefaultConfig()` の既定値を経由しない設定ファイルは意図せず起動を拒否されうる | P3 | ❌ UNFIXED | `config/config.go:23-38,61-83`。監査 §4 P3「R16」 |
| R17 | CI (`.github/workflows/ci.yml:37-41`) は `./tests/unit/...` と `./tests/integration/...` のみを `-race` 実行し、`./...`（各パッケージ直下の `_test.go`、例: `raft/installsnapshot_internal_test.go`）を対象にしない。また `tests/integration/cluster_test.go:393` の `break` は `select` から抜けるだけで外側の `for` ループを抜けないため、timeout 後も残りの `done` 受信を待ち続ける（意図した「テスト失敗で即座に打ち切る」動作になっていない） | P3 | ✅ FIXED | `19bdb37`（CI の 2 ステップを `go test -v -race -timeout=10m -coverprofile=coverage.txt -covermode=atomic ./...` の 1 ステップに統合、全パッケージ直下のテストを `-race` 対象化）、`91b0f7c`（`tests/integration/cluster_test.go` の timeout `break` をラベル付き `break waitLoop` に変更し、外側の for ループを確実に抜けるよう修正。SA4011 解消） |
| R18 | `examples/benchmark/benchmark.go` の read ワークロードはヒットしない: `populateInitialData`（`:152-162`、鍵生成は `:157`）が `i` を種に `Operations/populateFraction` 件を書き込む一方、読み取り側 `worker`（`:184-222`、鍵生成は `:201`）は `r.Intn(config.Operations/populateFraction)` で毎回ランダムな種を選ぶ。`generateKey` の乱数サフィックスは呼び出しごとに RNG ストリームが進むため、同じ数値シードでも書き込み時と読み取り時で鍵文字列が一致せず、GET はほぼ確実に 404 になる。成功率・引数検証・失敗統計もない | P3 | ❌ UNFIXED | `examples/benchmark/benchmark.go:152-162,184-222,285-287`。監査 §4 P3「R18」 |
| R19 | `RaftNode.Kill` は done を閉じるだけで run goroutine と replication goroutine の終了を待たない。呼び出し側が Kill 直後に `kvs.Close()` で applyCh を閉じると、進行中の tick／応答処理が閉じた applyCh に送信し `panic: send on closed channel` になる。CI（macOS）で `TestFullSystemPersistence_CrashAndRecover` が不安定失敗（PR #24 の run 34011107561）。監査 §4 P1-3（B3 と shutdown lifecycle）の範囲 | P1 | ✅ FIXED | `7c96f14`（`RaftState` が `stopCh` と `WaitGroup` を持ち、goroutine の起動は `spawn`（`raft/state.go`）の 1 経路に統一。対象は applier・`RaftNode.run`・`replicatePeerOnce`・`requestVoteFromPeer`・ReadIndex の `confirmLeadership` heartbeat・`TriggerSnapshot` の圧縮。`Kill` は `sync.Once` で冪等、`RaftState.Stop` で全 goroutine の終了を待ってから返る。Stop 開始後は `spawn` が false を返して何も起動せず、予約を持つ 2 箇所（`sendHeartbeats` の replication slot、`confirmLeadership` の results 枠）は自分で解放する。applier の送信は停止シグナルを先に見るので Kill 後は 1 件も送らない）＋ `13570d5`（`main.go` の停止順を HTTP API → transport → `Kill` → `kvs.Close()` に変更）。回帰テスト `raft/shutdown_internal_test.go` |

## 注記（レビュー後に判明した事実）

1. **B1 修正のトレードオフ（B3 として `f873d9b` で解消済み）**: ブロッキング送信化により、
   applyCh 逆圧時（例: applyLoop 内の同期スナップショット保存中）は rs.mu 保持のまま全 RPC・
   選挙処理が停止する liveness 問題に転化していた。専用 applier goroutine への分離（TODO.md
   の項目 2.5）でこれを解消した。送信がブロッキングであること自体は変わっていない — B1 の
   安全性（コミット済みエントリを落とさない）はそのままで、ブロックする場所がロックを持たない
   goroutine に移っただけである。付随して `LastApplied` の意味が「state machine に届いた」から
   「applier が確保した」に変わった（`raft/applier.go` の `takeApplyWork` 参照）。volatile な
   状態なので crash 時の扱いは従来どおり（境界まで巻き戻り、ログから再適用される）。
2. **`2a35ce9` の副作用（R2 として `c362ae4` で修正済み）**: AppendEntries でメモリ上のログを
   切り詰め+追記した後に persist が失敗すると `Success=false` を返すが、メモリとディスクの
   不一致が次回 persist 成功まで残っていた。
   **訂正（2026-09-06 再監査、R2）**: 「安全側」なのは persist が失敗した*その*要求への応答
   （`Success=false`）に限る。同一要求がリトライされた場合、`mergeLogEntries` は前回の
   （未永続の）メモリ上の追記と比較して「既に一致」と判定して `false` を返すため、persist 呼び出し
   自体がスキップされ、`Success=true` が返っていた。
   **修正済み（`c362ae4`）**: persist に失敗した AppendEntries は merge 前のログへロールバック
   するようになり、この不一致はハンドラを抜ける時点で残らない。したがって再送も改めて merge →
   persist を通り、成功するまで `Success=false` が返る。「変更のない duplicate 要求は persist
   しない」最適化は、メモリとディスクが一致している前提が成り立つため維持されている。同種の不一致が
   残っていた InstallSnapshot 受信経路も `0695b95`（R3）で同じ規律に揃えた（注記 6）。
3. **RequestVote の persist 失敗時**: メモリ上の VotedFor は保持したまま `VoteGranted=false`
   のみ返す。同一 term 内の再投票を防ぐ安全方向の意図的設計（`raft/rpc.go:94-104` のコメント参照）。
4. **A6 が CI で検出されなかった理由（解消済み）**: 旧統合テストは `fakeSnapshotter` を使い、
   (i) SetSnapshotter 未配線、(ii) インターフェース非互換、(iii) V2 形式 unmarshal 不整合の
   3 つのギャップをすべて迂回していた。`d0cbdc1`/`c516f54` で 3 ギャップを解消し、
   `tests/integration/snapshot_kvstore_wiring_test.go` が本番配線経路を検証するようになった。
5. **A7 修正で保持する suffix は backing array を共有する**: `logAfterSnapshot` は
   `persistent.Log` を再スライスして返すため（`TruncateLogAfter` と同じ方式）、破棄した
   エントリは配列上に残る。フォロワー受信経路であり E2（リーダー送信経路の race、`7e3eb61`
   で解消）とは別なので、この関数の戻り値をロック外へ持ち出す変更を加える際は改めて注意すること
   — E2 の対策（送信前のコピー）はこの経路には入っていない。
   なお R3（`0695b95`）の persist 失敗ロールバックは、この「上書きせず再スライスするだけ」
   という性質にそのまま依存している（保存しておいた slice header が有効な復元先になる）。
   `TruncateLogTo` の切り詰めも同様。
6. **InstallSnapshot の persist 失敗時（`0695b95` で解消）**: ログ置換・スナップショット境界・
   CommitIndex/LastApplied をメモリ上で更新した後に persist が失敗すると、適用は中止するが
   メモリ側の更新は残っていた（注記 2 と同種の不一致）。現在は R3 の修正で、persist 失敗時に
   Log／境界／CommitIndex／LastApplied をすべて呼び出し前の値へ戻す。ハンドラを抜ける時点で
   メモリとディスクが一致するという AppendEntries と同じ規律になった。ただし先に書いた
   KV payload（snapshot.json）はディスクに残る — これは意図的で、crash 状態を復旧可能な
   「snapshot が Raft より新しい」側に倒すための順序不変条件そのものである。
7. **R6 統一に伴う RequestVote の書き込み回数（`ac93fcb`）**: 高 term を見た時点で
   `becomeFollowerLocked` が term を persist し、投票を granted する場合はその後もう一度
   persist する（従来は末尾で 1 回）。選挙時のみ発生する 1 回分の増加であり、「応答前に
   durable」という規律を 1 箇所に置くためのトレードオフ。term persist が失敗しても
   ハンドラは抜けず投票判定まで進む — `persist()` は persistent state 全体を書くので、
   後段の書き込みが成功すれば term と vote の両方が durable になる。失敗した場合に
   メモリ上の VotedFor を保持するのは従来どおり（注記 3）。

## 修正の推奨順序

2026-07-07 報告書の推奨（B1 → C 群 → B2 → A 群 → D3/D1/D2 → D4/D5）はすべて完了し、
A 群（A1–A8）も A7 の修正で解消した。2026-09-06 再監査（グループ R）を踏まえた現在の推奨順は
`docs/raft-audit-2026-09-06.md` §6 のロードマップに従う:

1. 現状訂正のみ（文書・行番号・保証範囲） — 本 PR
2. ✅ 完了 — CI を `go test -race ./...` に拡大し、`tests/integration/cluster_test.go` の
   timeout `break` を修正（R17）
3. ✅ 完了 — R1（`Start` の atomic 化、`2c26b9a`）、R2（durable ACK、`c362ae4`）
4. ✅ 完了 — R3（世代整合・起動時検証、`0695b95`）、R4（payload/metadata の同一世代化、
   `f53617e`）、R5（古い snapshot の適用禁止、`156510a`）
5. ✅ 完了 — R6（`becomeFollowerLocked` への降格統一、`ac93fcb`）、E1（選挙タイマーの
   mutex 統一、`c5fdc0f`）、E2（送信エントリのコピー、`7e3eb61`）、peer 単位の送信直列化と
   進捗の単調化（`499c4b8`）
6. ✅ 完了 — B3（ordered applier、`f873d9b`）と shutdown lifecycle（R19・`7c96f14` /
   `13570d5`）
7. R9–R13（KV/client/API/RPC 細部）
8. R15（chunk transfer）
9. R14（joint consensus: state → quorum → 管理 API → snapshot の順）
10. R16、R18（設定、examples、benchmark）
