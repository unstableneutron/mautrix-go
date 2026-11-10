package bridgev2

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
)

type lifecycleClient struct {
	NetworkAPI
	connected   atomic.Bool
	connections atomic.Int32
}

func (c *lifecycleClient) Connect(context.Context) { c.connected.Store(true); c.connections.Add(1) }
func (c *lifecycleClient) Disconnect()             { c.connected.Store(false) }

type lifecycleNetwork struct {
	testNetwork
	load func(*UserLogin)
}

func (n *lifecycleNetwork) LoadUserLogin(_ context.Context, ul *UserLogin) error {
	n.load(ul)
	return nil
}

func TestResetAndReloginSerializeClientPublication(t *testing.T) {
	ctx := context.Background()
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "bridge.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	n := &lifecycleNetwork{}
	n.load = func(ul *UserLogin) {
		if loads.Add(1) == 2 {
			close(entered)
			<-release
		}
		ul.Client = &lifecycleClient{}
	}
	br := NewBridge("test", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{}, testMatrix{bot: &testIntent{}}, n, func(*Bridge) CommandProcessor { return nil })
	br.BackgroundCtx = ctx
	if err = br.DB.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := br.GetUserByMXID(ctx, testUserMXID)
	if err != nil {
		t.Fatal(err)
	}
	ul, err := u.NewLogin(ctx, &database.UserLogin{ID: "login", RemoteName: "old"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ul.BridgeState.Destroy)
	resetDone := make(chan struct{})
	go func() { br.ResetNetworkConnections(); close(resetDone) }()
	<-entered
	reloginDone := make(chan error, 1)
	replacement := &lifecycleClient{}
	go func() {
		_, err := u.NewLogin(ctx, &database.UserLogin{ID: "login", RemoteName: "replacement"}, &NewLoginParams{LoadUserLogin: func(_ context.Context, ul *UserLogin) error { ul.Client = replacement; return nil }})
		reloginDone <- err
	}()
	select {
	case err := <-reloginDone:
		close(release)
		<-resetDone
		t.Fatalf("replacement overtook unfinished recreation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-resetDone
	if err := <-reloginDone; err != nil {
		t.Fatal(err)
	}
	if ul.Client != replacement || ul.RemoteName != "replacement" {
		t.Fatal("reset published a stale client over replacement")
	}
	if row, err := br.DB.UserLogin.GetByID(ctx, "login"); err != nil || row.RemoteName != "replacement" {
		t.Fatal("replacement not durable")
	}
	// The initial login's asynchronous completion may arrive after reset has
	// already installed/connected a new client. It must not connect it twice.
	br.ResetNetworkConnections()
	current := ul.Client.(*lifecycleClient)
	ul.Connect(ctx)
	if current.connections.Load() != 1 {
		t.Fatal("delayed initial connection duplicated the reset client")
	}
}
