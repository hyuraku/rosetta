# Raft論文実装状況比較

本ドキュメントは [Raft論文](https://raft.github.io/raft.pdf) の内容と rosetta プロジェクトの実装状況を比較したものです。

## 概要サマリー

| カテゴリ | 状態 | 備考 |
|---------|------|------|
| リーダー選挙 (Section 5.2) | ✅ 実装済み | 完全実装 |
| ログ複製 (Section 5.3) | ✅ 実装済み | Fast rollback最適化含む |
| 安全性保証 (Section 5.4) | ✅ 実装済み | 選挙制限あり |
| 永続化 (Figure 2) | ✅ 実装済み | Persisterインターフェース |
| ログコンパクション (Section 7) | ✅ 実装済み | スナップショット対応 |
| クラスタメンバーシップ変更 (Section 6) | ❌ 未実装 | Joint consensus未対応 |
| クライアント相互作用 (Section 8) | ✅ 実装済み | 重複検知対応 |
| 読み取り専用クエリ最適化 | ✅ 実装済み | リースベースの読み取り |

---

## 詳細比較

### 1. リーダー選挙 (Section 5.2) ✅ 実装済み

#### 論文の要件
- サーバーはFollower → Candidate → Leaderの状態遷移を行う
- 選挙タイムアウトはランダム化してスプリットボートを防ぐ
- Candidateは自身に投票し、他ノードにRequestVote RPCを送信
- 過半数の票を得たらリーダーになる
- 任期(term)は論理クロックとして機能

#### 実装状況
**ファイル**: `raft/state.go`, `raft/rpc.go`, `raft/node.go`

```go
// raft/state.go:12-16 - 状態定義
const (
    Follower NodeState = iota
    Candidate
    Leader
)

// raft/state.go:97-99 - ランダム化された選挙タイムアウト (150-300ms)
randomTimeout := 150 + rand.Intn(150)

// raft/state.go:204-210 - リセット時も再ランダム化
func (rs *RaftState) ResetElectionTimer() {
    randomTimeout := 150 + rand.Intn(150)
    rs.electionTimeout = time.Duration(randomTimeout) * time.Millisecond
    ...
}
```

**RequestVote RPC構造体** (`raft/rpc.go:10-20`):
```go
type RequestVoteArgs struct {
    Term         int    // 候補者の任期
    CandidateID  string // 候補者ID
    LastLogIndex int    // 最後のログエントリのインデックス
    LastLogTerm  int    // 最後のログエントリの任期
}
```

✅ 論文の要件を完全に満たしています。

---

### 2. ログ複製 (Section 5.3) ✅ 実装済み

#### 論文の要件
- リーダーはクライアントコマンドをログに追加
- AppendEntries RPCでフォロワーにログを複製
- 過半数に複製されたらコミット
- nextIndex と matchIndex でフォロワーの状態を追跡
- ログ整合性チェック (prevLogIndex, prevLogTerm)

#### 実装状況
**ファイル**: `raft/rpc.go`, `raft/log.go`

```go
// raft/rpc.go:22-28 - AppendEntries RPC構造体
type AppendEntriesArgs struct {
    Term         int        // リーダーの任期
    LeaderID     string     // リーダーID
    PrevLogIndex int        // 直前のログエントリのインデックス
    PrevLogTerm  int        // 直前のログエントリの任期
    Entries      []LogEntry // 複製するエントリ
    LeaderCommit int        // リーダーのコミットインデックス
}

// raft/state.go:46-49 - リーダー状態
type LeaderState struct {
    NextIndex  map[string]int  // 各フォロワーへの次送信インデックス
    MatchIndex map[string]int  // 各フォロワーの複製済みインデックス
}
```

#### Fast Rollback最適化 ✅ 実装済み

論文のSection 5.3で言及されている最適化（フォロワーが競合情報を返してリーダーがスキップする）も実装済み:

```go
// raft/rpc.go:30-37
type AppendEntriesReply struct {
    Term    int
    Success bool
    // Fast rollback optimization (Section 5.3)
    ConflictTerm  int  // 競合エントリの任期
    ConflictIndex int  // ConflictTermの最初のインデックス
}
```

リーダー側の処理 (`raft/rpc.go:319-341`):
- `ConflictTerm == -1`: フォロワーのログが短い → `ConflictIndex`にジャンプ
- それ以外: リーダーのログで`ConflictTerm`を検索して最適な位置にスキップ

✅ 論文の要件と最適化を完全に満たしています。

---

### 3. 安全性保証 (Section 5.4) ✅ 実装済み

#### 論文の要件
- **選挙安全性**: 各任期で最大1人のリーダー
- **リーダー追記のみ**: リーダーはログを上書き・削除しない
- **ログ一致**: 同じインデックス・任期のエントリがあれば、それ以前も一致
- **リーダー完全性**: 過去にコミットされたエントリは選出されるリーダーに含まれる
- **状態機械安全性**: 適用されたエントリは全サーバーで同一結果

#### 実装状況

**選挙制限** (`raft/rpc.go:72-88`):
```go
// 候補者のログが自分より古い場合は投票しない
if args.LastLogTerm < lastLogTerm ||
   (args.LastLogTerm == lastLogTerm && args.LastLogIndex < lastLogIndex) {
    reply.VoteGranted = false
    return
}
```

✅ 論文の安全性保証を満たす実装になっています。

---

### 4. 永続化 (Figure 2) ✅ 実装済み

#### 論文の要件
- `currentTerm`, `votedFor`, `log[]` は永続化必須
- サーバー再起動時に復元

#### 実装状況
**ファイル**: `raft/state.go`

```go
// raft/state.go:31-39
type PersistentState struct {
    CurrentTerm int
    VotedFor    *string
    Log         []LogEntry
    // スナップショットメタデータ
    LastIncludedIndex int
    LastIncludedTerm  int
}

// raft/state.go:51-55
type Persister interface {
    SaveRaftState(state *PersistentState) error
    LoadRaftState() (*PersistentState, error)
}
```

✅ 論文の永続化要件を満たしています。

---

### 5. ログコンパクション / スナップショット (Section 7) ✅ 実装済み

#### 論文の要件
- スナップショットで状態機械の状態をキャプチャ
- スナップショット以前のログエントリを破棄
- InstallSnapshot RPCで遅れたフォロワーにスナップショットを送信

#### 実装状況
**ファイル**: `raft/snapshot.go`, `raft/rpc.go`

```go
// raft/rpc.go:40-46 - InstallSnapshot RPC
type InstallSnapshotArgs struct {
    Term              int    // リーダーの任期
    LeaderID          string
    LastIncludedIndex int    // スナップショットに含まれる最後のインデックス
    LastIncludedTerm  int    // そのエントリの任期
    Data              []byte // スナップショットデータ
}

// raft/snapshot.go - スナップショット関連メソッド
func (rs *RaftState) TakeSnapshot(...)
func (rs *RaftState) InstallSnapshotFromData(...)
func (rs *RaftState) ShouldTakeSnapshot(...) bool
```

**ApplyMsgのスナップショットサポート** (`raft/state.go:80-90`):
```go
type ApplyMsg struct {
    CommandValid bool
    Command      interface{}
    CommandIndex int
    // スナップショット用
    SnapshotValid bool
    SnapshotIndex int
    SnapshotTerm  int
    SnapshotData  []byte
}
```

✅ 論文のログコンパクション要件を満たしています。

---

### 6. クラスタメンバーシップ変更 (Section 6) ❌ 未実装

#### 論文の要件
- **Joint Consensus**: 新旧設定の両方で過半数を必要とする2段階プロセス
- Cold,new → Cnew の遷移
- 設定変更はログエントリとして複製

#### 現状
`network/discovery.go` に `ClusterManager` が存在しますが、これはHTTPレベルでのノード参加/離脱のみを処理:

```go
// network/discovery.go - HTTP経由のクラスタ管理
func (cm *ClusterManager) JoinCluster(existingNodeAddr string) error
func (cm *ClusterManager) LeaveCluster()
```

**問題点**:
- Raftレイヤーでの設定変更ログエントリなし
- Joint Consensus未実装
- メンバーシップ変更中の安全性保証なし

❌ **実装が必要**

---

### 7. クライアント相互作用 (Section 8) ✅ 実装済み

#### 論文の要件
- クライアントはリーダーにコマンドを送信
- リーダーは過半数に複製後、結果を返す
- **重複検知**: クライアントIDとシーケンス番号で再実行を防止
- **Linearizability**: 各操作は呼び出しと応答の間に一度だけ適用

#### 実装状況

**リーダーのみ書き込み** ✅:
- 非リーダーノードは503レスポンスでリダイレクト

**コマンド構造体** (`kvstore/store.go`):
```go
type Command struct {
    Op       Operation
    Key      string
    Value    string
    ID       string   // 操作追跡用ID
    ClientID string   // クライアント識別子 (重複検知用)
    SeqNum   int      // シーケンス番号 (重複検知用)
}
```

**クライアントセッション管理** (`kvstore/store.go`):
```go
type ClientSession struct {
    LastSeqNum int    // 最後に実行したシーケンス番号
    LastResult Result // キャッシュされた結果
}

type KVStore struct {
    ...
    sessions  map[string]*ClientSession // ClientID -> Session
    sessionMu sync.RWMutex
    ...
}
```

**重複検知ロジック** (`kvstore/store.go:executeCommand`):
- ClientIDが設定されている場合、重複チェックを実行
- `SeqNum < LastSeqNum`: 古いリクエスト → エラー返却
- `SeqNum == LastSeqNum`: 重複リクエスト → キャッシュから結果返却
- `SeqNum > LastSeqNum`: 新しいリクエスト → 実行してセッション更新

**クライアントライブラリ** (`kvstore/client.go`):
- 起動時にユニークなClientIDを生成
- 各操作でシーケンス番号をインクリメント
- Put/Delete操作でClientIDとSeqNumを送信

**スナップショット永続化** (`persistence/kv_snapshotter.go`):
- SnapshotterV2インターフェースでセッション情報も永続化
- 後方互換性あり（V1形式のスナップショットも読み込み可能）

✅ **論文のクライアント相互作用要件を完全に満たしています**

---

### 8. 読み取り専用クエリ最適化 ✅ 実装済み

#### 論文の要件
- リーダーは読み取りクエリをログ複製なしで処理可能
- ただし、リーダーシップ確認が必要:
  - ハートビート確認: クエリ前に過半数からの応答を確認
  - リース方式: ハートビート成功から一定時間内は安全

#### 実装状況
**ファイル**: `raft/state.go`, `raft/node.go`, `kvstore/store.go`

**リースベースの読み取り最適化を実装**:
- リーダーが過半数からのハートビート応答を受け取った時刻を追跡
- その時刻が選挙タイムアウト以内なら、Raftログを経由せずローカルから直接読み取り

```go
// raft/state.go - リーダーシップ確認の追跡
lastLeaderConfirmation time.Time // For read-only query optimization

// raft/state.go - 読み取り可能かチェック
func (rs *RaftState) CanServeReadOnlyQuery() bool {
    if rs.state != Leader {
        return false
    }
    // 選挙タイムアウト以内にリーダーシップが確認されていれば安全
    return time.Since(rs.lastLeaderConfirmation) < rs.electionTimeout
}

// kvstore/store.go - GETでの最適化利用
func (kvs *KVStore) Get(key string) (string, error) {
    if kvs.raft.CanServeReadOnlyQuery() {
        return kvs.getLocal(key) // ローカルから直接読み取り
    }
    // フォールバック: Raft経由
    return kvs.executeOperationWithResult(OpGet, key, "")
}
```

**ハートビート時の過半数確認** (`raft/rpc.go`):
```go
// sendHeartbeats内で成功応答をカウント
if int(newCount) >= majority {
    rs.lastLeaderConfirmation = time.Now()
}
```

✅ **リースベースの読み取り最適化を完全実装**

---

## 実装優先度の推奨

### 実装済み
1. ~~**重複検知の実装** (Section 8)~~ ✅
   - クライアントID + シーケンス番号でのべき等性保証
   - 再送時の重複適用防止
   - スナップショットでのセッション永続化

### 低優先度
2. **クラスタメンバーシップ変更**
   - Joint Consensus実装
   - 運用中のクラスタ拡張/縮小が必要な場合のみ

### 実装済み
- ~~読み取り専用クエリ最適化~~ ✅ リースベースで実装完了

---

## 参照

- [Raft論文 (PDF)](https://raft.github.io/raft.pdf)
- [Raft Visualization](https://raft.github.io/)
- [Diego Ongaro's PhD Dissertation](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf) - より詳細な仕様
