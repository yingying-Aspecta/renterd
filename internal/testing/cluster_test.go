package testing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/object"
	rhpv2 "go.sia.tech/renterd/rhp/v2"
	rhpv3 "go.sia.tech/renterd/rhp/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"lukechampine.com/frand"
)

// TestNewTestCluster is a test for creating a cluster of Nodes for testing,
// making sure that it forms contracts, renews contracts and shuts down.
func TestNewTestCluster(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	cluster, err := newTestCluster(t.TempDir(), zap.New(zapcore.NewNopCore()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cluster.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}()
	b := cluster.Bus
	w := cluster.Worker

	// Try talking to the bus API by adding an object.
	err = b.AddObject("/foo", object.Object{
		Key: object.GenerateEncryptionKey(),
		Slabs: []object.SlabSlice{
			{
				Slab: object.Slab{
					Key:       object.GenerateEncryptionKey(),
					MinShards: 1,
					Shards:    []object.Sector{}, // slab without sectors
				},
				Offset: 0,
				Length: 0,
			},
		},
	}, map[types.PublicKey]types.FileContractID{})
	if err != nil {
		t.Fatal(err)
	}

	// Try talking to the worker and request the object.
	err = w.DeleteObject("/foo")
	if err != nil {
		t.Fatal(err)
	}

	// See if autopilot is running by fetching the config.
	_, err = cluster.Autopilot.Config()
	if err != nil {
		t.Fatal(err)
	}

	// Add a host.
	if _, err := cluster.AddHosts(1); err != nil {
		t.Fatal(err)
	}

	// Wait for contracts to form.
	var contract api.Contract
	if contracts, err := cluster.WaitForContracts(); err != nil {
		t.Fatal(err)
	} else {
		contract = contracts[0]
	}

	// Verify startHeight and endHeight of the contract.
	currentPeriod, err := cluster.Autopilot.Status()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := cluster.Autopilot.Config()
	if err != nil {
		t.Fatal(err)
	}
	if contract.EndHeight() != currentPeriod+cfg.Contracts.Period+cfg.Contracts.RenewWindow {
		t.Fatal("wrong endHeight")
	}

	// Mine blocks until contracts start renewing.
	if err := cluster.MineToRenewWindow(); err != nil {
		t.Fatal(err)
	}

	// Wait for the contract to be renewed.
	err = Retry(100, 100*time.Millisecond, func() error {
		resp, err := w.ActiveContracts(time.Minute)
		if err != nil {
			return err
		}
		contracts := resp.Contracts
		if len(contracts) != 1 {
			return errors.New("no renewed contract")
		}
		if contracts[0].RenewedFrom != contract.ID {
			return fmt.Errorf("contract wasn't renewed %v != %v", contracts[0].RenewedFrom, contract.ID)
		}
		if contracts[0].EndHeight() != contract.EndHeight()+cfg.Contracts.Period {
			t.Fatal("wrong endHeight")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUploadDownload is an integration test that verifies objects can be
// uploaded and download correctly.
func TestUploadDownload(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// sanity check the default settings
	if defaultAutopilotConfig.Contracts.Hosts < uint64(defaultRedundancy.MinShards) {
		t.Fatal("too few hosts to support the redundancy settings")
	}

	// create a test cluster
	cluster, err := newTestCluster(t.TempDir(), zap.New(zapcore.NewNopCore()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cluster.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}()

	w := cluster.Worker
	rs := defaultRedundancy

	// add hosts
	if _, err := cluster.AddHostsBlocking(int(rs.TotalShards)); err != nil {
		t.Fatal(err)
	}

	// prepare two files, a small one and a large one
	small := make([]byte, rhpv2.SectorSize/12)
	large := make([]byte, rhpv2.SectorSize*3)

	for _, data := range [][]byte{small, large} {
		// prepare some data - make sure it's more than one sector
		if _, err := frand.Read(data); err != nil {
			t.Fatal(err)
		}

		// upload the data
		name := fmt.Sprintf("data_%v", len(data))
		if err := w.UploadObject(bytes.NewReader(data), name); err != nil {
			t.Fatal(err)
		}

		// download the data
		var buffer bytes.Buffer
		if err := w.DownloadObject(&buffer, name); err != nil {
			t.Fatal(err)
		}

		// assert it matches
		if !bytes.Equal(data, buffer.Bytes()) {
			t.Fatal("unexpected")
		}
	}
}

// TestEphemeralAccounts tests the use of ephemeral accounts.
func TestEphemeralAccounts(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	cluster, err := newTestCluster(t.TempDir(), zap.New(zapcore.NewNopCore()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cluster.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}()
	w := cluster.Worker

	// add host
	nodes, err := cluster.AddHosts(1)
	if err != nil {
		t.Fatal(err)
	}
	host := nodes[0]
	hg, err := host.HostGet()
	if err != nil {
		t.Fatal(err)
	}

	// Wait for contracts to form.
	var contract api.Contract
	if contracts, err := cluster.WaitForContracts(); err != nil {
		t.Fatal(err)
	} else {
		contract = contracts[0]
	}

	// Account shouldnt' exist.
	accounts, err := w.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("wrong number of accounts %v", len(accounts))
	}

	// Fund account.
	if err := w.RHPFund(contract.ID, contract.HostKey(), types.Siacoins(1)); err != nil {
		t.Fatal(err)
	}

	// Expected account balance should have increased.
	accounts, err = w.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("wrong number of accounts %v", len(accounts))
	}
	acc := accounts[0]
	if acc.Balance.Cmp(types.Siacoins(1).Big()) != 0 {
		t.Fatalf("wrong balance %v", acc.Balance)
	}
	if acc.ID == (rhpv3.Account{}) {
		t.Fatal("account id not set")
	}
	if acc.Host != types.PublicKey(hg.PublicKey.ToPublicKey()) {
		t.Fatal("wrong host")
	}
	if acc.Owner == "" {
		t.Fatal("owner not set")
	}

	// Fetch account from bus directly.
	busAccounts, err := cluster.Bus.Accounts("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(busAccounts) != 1 {
		t.Fatal("expected one account but got", len(busAccounts))
	}
	busAcc := busAccounts[0]
	if !reflect.DeepEqual(busAcc, acc) {
		t.Fatal("bus account doesn't match worker account")
	}
}
