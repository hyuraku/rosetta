# Known Issues — 既知の安全性問題

> 最終検証: 2026-08-31 / 対象 commit `019d33e`
>
> This file is the **live, authoritative status** of the safety issues found in the
> 2026-07-07 safety review. The frozen report with full evidence and reproduction
> scenarios is [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md).
> Update this file (not the report) whenever an issue's status changes.

このファイルは [docs/safety-review-2026-07-07.md](docs/safety-review-2026-07-07.md)
（2026-07-07 時点の安全性レビュー報告書・凍結）で確認された問題の**現在のステータス表**です。
修正が main にマージされたら、該当行の状態と修正 commit をこのファイルで更新してください。
報告書そのものは編集しないこと。

## サマリ

| 状態 | 件数 |
|---|---|
| ✅ FIXED | 19（A1–A8, B1, B2, C1, C2, C3, C4, D1, D2, D3, D4, D5） |
| 🟠 PARTIAL | 0 |
| ❌ UNFIXED | 3（B3, E1, E2） |

**実用上の含意**: ログ圧縮（グループ A）の安全性課題は A7 の修正（`019d33e`）ですべて解消し、
圧縮を有効にしても Log Matching は破れなくなった。ただし同じ InstallSnapshot 受信経路には
B3（`rs.mu` 保持のまま `applyCh` へブロッキング送信）の **liveness** リスクが残るため、
圧縮を有効にする場合はスナップショット受信中にノード全体が停止しうる点を承知して使うこと。
読み取りは ReadIndex 化により線形化された（D1–D3 解消。選挙直後は当選時 no-op が
コミットされるまで一時的に読みが待たされる）。クライアントのリトライは D4/D5 修正で
at-most-once 化済み。

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

## 注記（レビュー後に判明した事実）

1. **B1 修正のトレードオフ**: ブロッキング送信化により、applyCh 逆圧時（例: applyLoop 内の
   同期スナップショット保存中）は rs.mu 保持のまま全 RPC・選挙処理が停止する liveness 問題に
   転化した。専用 applier goroutine への分離が対策（TODO.md の項目 2.5）。
2. **`2a35ce9` の副作用**: AppendEntries でメモリ上のログを切り詰め+追記した後に persist が
   失敗すると `Success=false` を返すが、メモリとディスクの不一致が次回 persist 成功まで残る
   （安全側の挙動）。
3. **RequestVote の persist 失敗時**: メモリ上の VotedFor は保持したまま `VoteGranted=false`
   のみ返す。同一 term 内の再投票を防ぐ安全方向の意図的設計（`raft/rpc.go:96-105` のコメント参照）。
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

報告書の推奨（B1 → C 群 → B2 → A 群 → D3/D1/D2 → D4/D5）はすべて完了し、A 群（A1–A8）も A7 の修正で解消した。
残りは **E1/E2 → B3** の順を推奨。E1・E2 は小さな data race で修正コストが低く、B3 は
TODO.md の「2.5. Decouple Log Application into a Dedicated Applier Goroutine」と同じ根
（`rs.mu` 保持下の `applyCh` 送信）なので、まとめて対応するのが自然。
