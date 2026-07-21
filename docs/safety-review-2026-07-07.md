# Raft 安全性レビュー報告書（2026-07-07）

**対象**: `raft/`・`kvstore/`・`persistence/`（計約3,500行）の全文精読による、Raft論文
（extended version, Ongaro & Ousterhout）の5つの安全性特性（Election Safety /
Leader Append-Only / Log Matching / Leader Completeness / State Machine Safety）への
適合性レビュー。

**方法**: 一次読解で抽出した21件の懸念すべてに対し、独立した3系統の敵対的検証
（「本当に安全性違反か」の反証を試みるレビュー）を実施。
**16件が CONFIRMED（違反シナリオ成立）、1件が PLAUSIBLE、4件は反証され安全と確認**。

## 結論の要約

現状の実装は「単一リーダーが安定稼働し、圧縮もクラッシュも起きない」ハッピーパスでは
動作するが、以下のいずれかが起きると Election Safety・Log Matching・Leader Completeness・
State Machine Safety の4特性が実際に破れる。

1. ログ圧縮が一度でも発火する（`MaxRaftState > 0` なら通常運転で自動発火）
2. 投票直後にクラッシュする
3. AppendEntries が重複・遅延配送される（HTTPトランスポートに順序保証・重複排除なし）
4. リーダー交代直後に読み取る

特にログ圧縮起因のバグ群（グループA）は「稀なタイミング依存」ではなく通常運転で踏む。

---

## 【致命】グループA: ログ圧縮のインデックス体系が半分しか実装されていない

リーダーの送信側（`raft/rpc.go:280-305`）だけが「絶対インデックス（スナップショット込み）」と
「相対位置（切り詰め後のスライス位置）」の変換を行い、**受信ハンドラ・投票・コミット判定・
適用の全経路が未対応**。フォロワーも `kvstore/store.go:276-291` の applyLoop で圧縮するため、
全ノードが被害者になる。

### A1. 圧縮済みフォロワーへの AppendEntries が壊れる — CONFIRMED
- **根拠**: `raft/rpc.go:115`（`PrevLogIndex > len(Log)`）、`:122`（`Log[PrevLogIndex-1]`）、
  `:136`（`Log[:PrevLogIndex]`）、`:147`（`min(LeaderCommit, len(Log))`）のいずれも
  `LastIncludedIndex` オフセット無し。リーダー側は絶対インデックスを送る（`rpc.go:294-295,351`）。
- **再現**: フォロワーが index 100 で圧縮（ログ残5件）→ リーダーが PrevLogIndex=105 を送信
  → `105 > 5` で ConflictIndex=6（相対）を返答 → リーダーは NextIndex=6 と解釈し絶対 index 6 から
  再送 → フォロワーはスライス位置5（絶対105）の後ろにコミット済みエントリを重複 append し、
  `rpc.go:140-141` で Index フィールドを相対値に書き換え → ログ恒久破壊。

### A2. 全圧縮済みノードが「空ログ」を名乗って投票する — CONFIRMED（Leader Completeness 違反）
- **根拠**: `raft/rpc.go:77-81`（RequestVote）と `:163-167`（startElection）は
  `lastLogIndex=len(Log)`・末尾term（空なら0）を使い、LastIncludedIndex/Term を無視。
  `GetLastLogIndexWithSnapshot` / `GetLastLogTermWithSnapshot`（`raft/snapshot.go:189-206`）は
  実装済みだが投票経路で未使用。
- **再現**: A,B が100件コミットし全圧縮（lastLogTerm=0 を名乗る）。index 10 以降分断されていた
  C（lastLogTerm=1, lastLogIndex=10）が立候補 → A,B は `args.LastLogTerm(1) > 0` で投票
  （`rpc.go:83`）→ コミット済み 11-100 を持たない C がリーダーに。コミット済みデータ喪失。

### A3. 圧縮済みノードが当選するとクラスタが書き込み不能になる — CONFIRMED
- **根拠**: `raft/state.go:235` で `NextIndex = len(Log)+1`（相対）を設定するが、
  `raft/rpc.go:290-298` はこれを絶対値として `lastIncludedIndex` と比較。
- **再現**: LastIncludedIndex=100・ログ残5件のノードが当選 → NextIndex=6 →
  `prevLogIndex(5) < lastIncludedIndex(100)` で全ピアに sendSnapshot 判定。production では
  snapshotter が nil（A6）のため `rpc.go:308-310` で即 return し、**新リーダーはハートビートを
  一切送れない** → 選挙が無限に繰り返され書き込み不能。

### A4. リーダー圧縮後にコミットが恒久停止する — CONFIRMED
- **根拠**: `raft/rpc.go:429` の `for n := CommitIndex+1; n <= len(Log)` — CommitIndex は絶対値、
  `len(Log)` は相対長。`TruncateLogTo`（`raft/snapshot.go:151-179`）は volatile を更新しない。
- **再現**: リーダーが index 100 で圧縮 → CommitIndex=100 > len(Log)=5 → ループ本体が一度も
  実行されずコミット停止。クライアントの Put は全て5秒タイムアウト（`kvstore/store.go:483-487`）。

### A5. 再起動後に誤適用・二重適用・範囲外 panic — CONFIRMED
- **根拠**: `raft/state.go:119` で volatile={0,0} に初期化され、LastApplied がスナップショットから
  復元されない。`raft/log.go:230` は `Log[LastApplied-1]` をスライス位置として使用。
  kvstore 側にも `CommandIndex` の単調性チェックなし（`kvstore/store.go:257-262`）。
- **再現**: 圧縮済みノード再起動 → kvstore はスナップショット（lastAppliedIndex=N）をロード済み
  なのに raft は LastApplied=0 から再適用 → スナップショットに含まれる suffix の二重適用で
  セッションテーブルが汚染。InstallSnapshot 後の合流経路では
  LastApplied（絶対）> len(Log)（相対）となり `Log[LastApplied-1]` の範囲外 panic も到達可能。

### A6. production で InstallSnapshot が永久に発火しない／発火しても必ず失敗 — CONFIRMED
- **根拠(a)**: `raftNode.SetSnapshotter` の呼び出しはリポジトリ全体で0件（`main.go:220-235` は
  kvstore にのみ配線）。しかも `persistence.KVSnapshotter` は `raft.Snapshotter`
  （`raft/snapshot.go:16-27`）を実装していないため、そもそも渡せない。
  → 圧縮境界より遅れたフォロワーは永久にキャッチアップ不能。
- **根拠(b)**: 仮に配線されても、V2形式スナップショット（`{"kv_data":...,"sessions":...}`、
  `persistence/kv_snapshotter.go:41-43`）を `map[string]string` として unmarshal するため必ず失敗
  （`kvstore/store.go:298-302`）。raft 側は既にログ破棄・CommitIndex/LastApplied 前進・persist 済み
  （`raft/rpc.go:526-541`）→ 状態機械の恒久データ欠落。

### A7. InstallSnapshot 受信側が分岐した suffix を term 検査なしで保持 — CONFIRMED（Log Matching 違反）
- **根拠**: `raft/rpc.go:519-526` は `entry.Index > args.LastIncludedIndex` のみで無条件保持。
  論文§7は「スナップショット最終エントリと index/term 両方一致なら以降を保持、さもなくば全破棄」。
- **再現**: F が旧リーダー(term2)の分岐エントリ 5..12 を保持 → 新リーダー(term3)が index 10 で
  圧縮しスナップショット送信 → F は分岐した 11,12(term2) を保持 → リーダーは
  NextIndex=11, MatchIndex=10 と誤認（`rpc.go:343-344`）→ ハートビートで LeaderCommit≥12 が
  届くと分岐エントリがコミット・適用される（`rpc.go:146-148`）。

### A8. フォロワー側スナップショットの永続化欠如 → クラッシュで恒久復元不能 — CONFIRMED
- **根拠**: InstallSnapshot 受信で raft 状態は persist される（`raft/rpc.go:541`）が、
  kvstore 側 `installSnapshotFromApplyMsg`（`kvstore/store.go:296-312`）はメモリ更新のみで
  `saveSnapshot` を呼ばない。
- **再現**: InstallSnapshot 受信・persist 直後にクラッシュ → 再起動: raft ログは N+1 から、
  KV スナップショットファイルは古い/無い → リーダーは `rpc.go:515` と `rpc.go:294-297` により
  再送しない → **index ≤ N の状態を恒久的に復元不能**。

---

## 【致命】グループB: コミット済みエントリの喪失・状態機械の発散

### B1. applyCh 満杯時にコミット済みコマンドを黙って捨てる — CONFIRMED（最重症）
- **根拠**: `raft/log.go:238-241` の `select { case rs.applyCh <- applyMsg: default: }`。
  直前の `:229` で `LastApplied++` 済みのため**再送機構が存在しない**。applyCh 容量は100
  （`kvstore/store.go:87`）。
- **現実的な発火条件**: applyLoop 内の同期スナップショット保存（`kvstore/store.go:282`、
  JSON marshal + ファイル書き込み）でチャネル消費が止まっている間に、キャッチアップ中の
  フォロワーへ LeaderCommit が100超ジャンプすると `rpc.go:146-149` が rs.mu 保持のまま一気に
  push → 101個目以降が drop。
- **帰結**: レプリカ間で状態が恒久発散（State Machine Safety 違反相当）。後続 ApplyMsg で
  `lastAppliedIndex` は先へ進むため検知もされず、そのノードのスナップショットに欠損が固定化。
  当該ノードが後にリーダー化しリース読みを返すと ACK 済み書き込みの欠けた値を返す。

### B2. AppendEntries の無条件切り詰めでコミット済み suffix が消える — CONFIRMED
- **根拠**: `raft/rpc.go:134-138` — 一致確認（consistency check は PrevLogIndex **以前**しか
  見ない）なしに `Log[:PrevLogIndex]` へ切り詰め+追記。論文§5.3 step 3
  「conflict する場合のみ削除」に違反。HTTPトランスポート（`network/transport.go`）は
  順序保証・重複排除がなく、リーダー側50msタイムアウト（`rpc.go:357`）後もサーバ側処理は
  完走するため、同一ピア宛の複数 AppendEntries が同時 in-flight になり古い方が後から届きうる。
- **再現**: ① L→A に AE₁(prev=0, entries=[e1]) が遅延。② AE₂(prev=0, entries=[e1,e2,e3]) が
  先に到着し append、L がコミット（commitIndex=3、A も適用済み）。③ 遅延した AE₁ 到着 →
  A のログが [e1] に切り詰め。④ 直後に L クラッシュ。⑤ e2,e3 を持たないノードが当選 →
  **ACK 済みの e2,e3 が恒久喪失**（Leader Completeness / State Machine Safety 違反）。
- **反証で確認済みの限定**: L が生きていれば次のハートビートで自己修復する。また切り詰め後に
  commitIndex > len(Log) となるが applyEntries は同期適用のため panic には至らない
  （ただし `rpc.go:147` で CommitIndex が逆行する — これ自体も論文違反）。

### B3. InstallSnapshot が rs.mu 保持のまま applyCh へブロッキング送信 — PLAUSIBLE
- **根拠**: `raft/rpc.go:490-491,544`。満杯時は全 RPC・選挙・ハートビートが停止。
  消費側 applyLoop は rs.mu を要求しないため循環待ちにはならず、消費が進めば回復する
  （恒久デッドロックは applyLoop 停止/Close 時のみ）。ロック保持下のブロッキング送信自体は欠陥。

---

## 【重大】グループC: 永続化の欠落による Election Safety 違反

### C1. RequestVote が term・votedFor を一切 persist しない — CONFIRMED
- **根拠**: `raft/rpc.go:59-92` — CurrentTerm 更新(:71)・VotedFor 更新(:85) 後に `rs.persist()` なし。
  persist する `SetVotedFor`/`IncrementTerm`（`raft/state.go:196-209`）はこの経路から呼ばれない。
- **再現**: 3ノード {A,B,C}。A が term5 で候補者 B に投票（VoteGranted 返信）→ クラッシュ →
  再起動（永続状態は term4/votedFor=nil）→ term5 の候補者 C にも投票可能。B と C がそれぞれ
  (自票+A票) で過半数=2 を満たし、**同一 term に2リーダー**（Election Safety 違反）。

### C2. ハートビート経由の term 更新が persist されない — CONFIRMED（実害は C1 との合わせ技）
- **根拠**: `raft/rpc.go:105-108` の term 更新に対し、persist は entries 非空時の `:143` のみ。
  リーダー側の返信処理 `:333-337`・`:368-372` も同様に欠落（`startElection` 内 `:219-224` のみ persist あり）。
- 論文の永続化要件（currentTerm is persistent）違反として確定。単独での2リーダー成立は C1 依存。

### C3. persist() のエラー無視 — CONFIRMED
- **根拠**: `raft/state.go:156-164` — `SaveRaftState` のエラーはログ出力のみ。呼び出し側は
  persist の成否に関係なく `Success=true` を返す（`rpc.go:151`）。`AppendLogEntry`
  （`raft/log.go:124-138`）も同様。
- **再現**: 5ノード。フォロワー D,E のディスクが書き込み不能でも Success=true → リーダーが
  コミットし ACK → リーダー+D+E が電源断 → 再起動後、当該エントリを持つノードが存在しない
  → コミット済みエントリ喪失。

### C4. 永続状態ロード失敗で「記憶喪失ノード」として参加 — CONFIRMED
- **根拠**: `raft/state.go:129-133` — ロード失敗を警告のみで握り潰し term=0/votedFor=nil で起動継続。
  `persistence/file_storage.go:97-106` は読み取りエラー・JSON 破損で error を返すが、それが無視される。
- **再現**: 投票済みノードの `raft_state.json` が破損 → 再起動で term0 参加 → 同一 term に再投票
  → C1 と同じ二重投票。正しくは起動拒否。

---

## 【重大】グループD: 読み取りの linearizability 違反

### D1. リース期間にリーダー自身のランダム electionTimeout を流用 — CONFIRMED
- **根拠**: `raft/state.go:293`（`time.Since(lastLeaderConfirmation) < rs.electionTimeout`）。
  timeout は 150–300ms を都度再抽選、フォロワーは独立抽選。クロックドリフト余裕なし。
  過半数喪失での step-down も存在しない（`rpc.go:360-363` は err を単に return）。
- **再現タイムライン**（L の timeout=300ms、F1=150ms）:
  - t=0: L のハートビート応答が過半数到達、lastLeaderConfirmation=now（`rpc.go:388`）
  - t=0+ε: L がパーティションで孤立（state=Leader のまま）
  - t≈150ms: F1 が選挙タイムアウト → term+1 で当選
  - t≈160ms: 新リーダー F1 が PUT x=2 をコミットしクライアントに ACK
  - t=170ms: クライアントが L に GET → `CanServeReadOnlyQuery()`=true（170<300）→
    `getLocal`（`kvstore/store.go:421-430`）が旧値 x=1 を返す。**ACK 済み書き込みを読み逃す**。
    ステイル窓は最大 ~150ms + D2 の応答遅延分。

### D2. リース起点が「応答受信時刻」 — CONFIRMED
- **根拠**: `raft/rpc.go:385-389`。フォロワーの選挙タイマーはハートビート**受信**時に走り始める
  （`rpc.go:112`）ため、正しいリース起点は送信時刻。応答遅延（ctx timeout 50ms）分リースが
  不当に延長され、D1 の違反窓を最大 ~50ms 拡大。

### D3. 当選時の no-op エントリがない — CONFIRMED（最も再現容易な違反）
- **根拠**: `raft/rpc.go:227-240`（当選処理に no-op 追加なし）、`:429-431`（現 term のエントリしか
  コミットカウントしない — §5.4.2 準拠だが、no-op がないと前任のコミット済みエントリを advance
  できない）。リース成立は commitIndex と無関係（`rpc.go:386-388`）。論文§8が明示要求する
  「就任時 no-op コミット」の欠落。
- **再現**: term1 の L1 が index5 (PUT x=1) を過半数複製し commitIndex=5 で ACK →
  LeaderCommit=5 を配る前にクラッシュ → F2（index5 保持、commitIndex=4、未適用）が term2 で当選
  → 最初のハートビート往復でリース成立 → GET x が **ACK 済みの x=1 を含まない状態**を返す。
  term2 の新規書き込みがコミットされるまで継続。パーティション不要。

### D4. 重複検出機構が実 API から完全に未配線 — CONFIRMED
- **根拠**: `kvstore/store.go:450`（opID = nodeID + UnixNano、ClientID/SeqNum 未設定）。
  dedup は `cmd.ClientID != ""` の場合のみ発動（`:316-320`）→ sessions/checkDuplicateRequest
  （`:334-356`）は実 API 経路でデッドコード。
- **再現**: PUT → ログ append・複製直後にリーダーが過半数と分断 → 5秒タイムアウトでエラー応答
  （`:483-487`）、しかしログエントリは残る → 後に新リーダー経由でコミット・適用
  （エラー応答済みなのに状態変化）→ クライアントリトライが別エントリとして再適用 = 二重適用。
  DELETE→リトライ PUT 復活、他クライアント介在時の lost update も成立。

### D5. 結果受領後の spurious な "leadership lost" エラー — CONFIRMED（実装特有の欠陥）
- **根拠**: `kvstore/store.go:477-481`。resultCh から結果を**受領済み** = 当該エントリは適用済み
  = コミット確定済み = 操作は確定的に成功しているのに、term/leader 再確認でエラーを返す。
  Raft 固有の曖昧さ（結果不明時のみ許容）ではない。この誤エラーがリトライを誘発し、
  D4 と組み合わさって能動的に二重適用を生む。

---

## 【中】グループE: data race（`go test -race` で検出可能な見込み）

### E1. startElection がロック外で ResetElectionTimer を呼ぶ — CONFIRMED（race として）
- **根拠**: `raft/rpc.go:172` で Unlock 後、`:174` で `ResetElectionTimer()`。
  `ResetElectionTimer`（`raft/state.go:217-223`）は内部でロックを取らず `electionTimeout` /
  `lastHeartbeat` を書く。他の呼び出し元（`rpc.go:87,112,512`）は Lock 下。
  `CanServeReadOnlyQuery`（`state.go:293`）の RLock 読みと競合。安全性への追加実害は小。

### E2. ハートビート送信エントリと AppendEntries ハンドラの backing array 共有 — CONFIRMED（race として）
- **根拠**: `raft/rpc.go:300-305` で `entries := rs.persistent.Log[...]` を取得後 RUnlock し、
  ロック外で JSON marshal。リーダー転落直後に受けた AppendEntries の上書き+Index 書き換え
  （`rpc.go:136-141`）と競合し、新旧混合の壊れたエントリが送信されうる。
  （`TruncateLogTo` 単独は re-slice のみで要素を変異しないため無害 — 検証で確認済み。）

---

## 反証され「安全」と確認できた点

- **リース失効後の Raft 経由 GET フォールバック**（`kvstore/store.go:412`）: 孤立リーダーでは
  過半数が取れず必ず5秒タイムアウトエラーで終端。古い値を返すパスは存在しない。
- **リース読みに「適用待ち（readIndex）」がない点**: ACK が applyLoop の実行**後**に返る設計のため、
  同一リーダー上では ACK 済み書き込みは必ず観測される。単体では違反にならない
  （ただし B1・D3 が修正されている前提でのみ成立する条件付きの安全性）。
- **B2 での commitIndex > len(Log) による panic**: applyEntries が同一クリティカルセクション内で
  同期適用するため到達しない。
- **`getLocal` と applyLoop の `kvs.mu` 排他**（`kvstore/store.go:360,422`): 正しく排他されている。

---

## 統合テスト設計案（`tests/integration/` 向け、未実装）

既存の `snapshot_compaction_test.go` のパターン（MockTransport + late joiner）を踏襲。

1. **TestVotePersistenceAcrossCrash**（C1/C4）: 投票 → `Kill()` → 同じ persister で再生成 →
   同 term の別候補の RequestVote に `VoteGranted=false` を検証。`raft_state.json` を破損させ
   「起動拒否」も検証。
2. **TestDuplicateAppendEntriesDoesNotTruncateCommitted**（B2）: `AE(prev=0, [e1,e2,e3])` →
   コミット → 遅延を模して `AE(prev=0, [e1])` を再送 → ログ長が3のままであることを検証。
   MockTransport に遅延・重複配送を注入するデコレータを追加すると系統的に叩ける。
3. **TestCompactedFollowerAcceptsReplication**（A1/A4）: 100件コミット → フォロワーで
   `TruncateLogTo(95)` → さらに10件書き込み → 全ノードの適用列一致と commitIndex 前進を検証。
4. **TestCompactedNodeVoteSafety**（A2）: 全圧縮済み2ノード + 古いログのみの1ノードで選挙し、
   古いノードが当選**できない**ことを検証。
5. **TestApplyChannelNeverDrops**（B1）: applyCh の消費を止めた状態で150件コミット → 消費再開 →
   全150件が順序どおり届くことを検証。
6. **TestLeaseReadAfterPartition**（D1/D3）: MockTransport にパーティション機能を追加。
   旧リーダー孤立 → 新リーダーが PUT → ACK → 旧リーダーのリース読みが新リーダーのコミット
   時点以降 false であること、および新リーダーが当選直後に前任コミット分を返せること
   （no-op 導入後）を検証。
7. **TestFollowerSnapshotDurability**（A8）: InstallSnapshot 受信直後にプロセス再生成し、
   KV 状態が復元できることを検証。
8. **`-race` の常時有効化**（E1/E2）: `make test-race` を圧縮・選挙系テストで回すだけで
   検出できる見込み。

## 修正の推奨順序

1. **B1** — applyCh drop の撤廃（LastApplied 前進と送信の分離）。発散したデータは他のあらゆる
   修正を無意味にするため最優先。
2. **C1/C2/C3/C4** — persist 規律の確立: 応答を返す前に persist、persist 失敗時は応答も失敗、
   ロード失敗時は起動拒否。
3. **B2** — AppendEntries step 3 の一致確認（既存エントリと index/term が一致する場合は
   切り詰めない）。
4. **A群** — 圧縮の全面改修（絶対/相対インデックスの統一、投票・コミット・適用経路の対応、
   raft.Snapshotter の実配線、フォロワー側スナップショット永続化）。当面は `MaxRaftState=0` で
   圧縮を無効化するのが安全な暫定策。
5. **D3 → D1/D2** — 当選時 no-op の導入、リース設計の見直し（確実な線形化が必要なら
   readIndex 方式への切り替えを推奨）。
6. **D4/D5** — ClientID/SeqNum の実配線と、結果受領時の正しい応答処理。
