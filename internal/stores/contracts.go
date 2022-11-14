package stores

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"go.sia.tech/renterd/internal/consensus"
	rhpv2 "go.sia.tech/renterd/rhp/v2"
	"go.sia.tech/renterd/slab"
	"go.sia.tech/siad/types"
	"gorm.io/gorm"
)

// EphemeralContractStore implements api.ContractStore and api.HostSetStore in memory.
type EphemeralContractStore struct {
	mu        sync.Mutex
	contracts map[types.FileContractID]rhpv2.Contract
	hostSets  map[string][]consensus.PublicKey
}

// Contracts implements api.ContractStore.
func (s *EphemeralContractStore) Contracts() ([]rhpv2.Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cs []rhpv2.Contract
	for _, c := range s.contracts {
		cs = append(cs, c)
	}
	return cs, nil
}

// Contract implements api.ContractStore.
func (s *EphemeralContractStore) Contract(id types.FileContractID) (rhpv2.Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contracts[id]
	if !ok {
		return rhpv2.Contract{}, errors.New("no contract with that ID")
	}
	return c, nil
}

// AddContract implements api.ContractStore.
func (s *EphemeralContractStore) AddContract(c rhpv2.Contract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contracts[c.ID()] = c
	return nil
}

// RemoveContract implements api.ContractStore.
func (s *EphemeralContractStore) RemoveContract(id types.FileContractID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contracts, id)
	return nil
}

// HostSets implements api.HostSetStore.
func (s *EphemeralContractStore) HostSets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sets := make([]string, 0, len(s.hostSets))
	for set := range s.hostSets {
		sets = append(sets, set)
	}
	return sets
}

// HostSet implements api.HostSetStore.
func (s *EphemeralContractStore) HostSet(name string) []consensus.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostSets[name]
}

// SetHostSet implements api.HostSetStore.
func (s *EphemeralContractStore) SetHostSet(name string, hosts []consensus.PublicKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(hosts) == 0 {
		delete(s.hostSets, name)
	} else {
		s.hostSets[name] = append([]consensus.PublicKey(nil), hosts...)
	}
	return nil
}

// NewEphemeralContractStore returns a new EphemeralContractStore.
func NewEphemeralContractStore() *EphemeralContractStore {
	return &EphemeralContractStore{
		contracts: make(map[types.FileContractID]rhpv2.Contract),
	}
}

// JSONContractStore implements api.ContractStore in memory, backed by a JSON file.
type JSONContractStore struct {
	*EphemeralContractStore
	dir string
}

type jsonContractsPersistData struct {
	Contracts []rhpv2.Contract
	HostSets  map[string][]consensus.PublicKey
}

func (s *JSONContractStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p jsonContractsPersistData
	for _, c := range s.contracts {
		p.Contracts = append(p.Contracts, c)
	}
	p.HostSets = s.hostSets
	js, _ := json.MarshalIndent(p, "", "  ")

	// atomic save
	dst := filepath.Join(s.dir, "contracts.json")
	f, err := os.OpenFile(dst+"_tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0660)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(js); err != nil {
		return err
	} else if err := f.Sync(); err != nil {
		return err
	} else if err := f.Close(); err != nil {
		return err
	} else if err := os.Rename(dst+"_tmp", dst); err != nil {
		return err
	}
	return nil
}

func (s *JSONContractStore) load() error {
	var p jsonContractsPersistData
	if js, err := os.ReadFile(filepath.Join(s.dir, "contracts.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	} else if err := json.Unmarshal(js, &p); err != nil {
		return err
	}
	for _, c := range p.Contracts {
		s.contracts[c.ID()] = c
	}
	s.hostSets = p.HostSets
	return nil
}

// AddContract implements api.ContractStore.
func (s *JSONContractStore) AddContract(c rhpv2.Contract) error {
	s.EphemeralContractStore.AddContract(c)
	return s.save()
}

// RemoveContract implements api.ContractStore.
func (s *JSONContractStore) RemoveContract(id types.FileContractID) error {
	s.EphemeralContractStore.RemoveContract(id)
	return s.save()
}

// SetHostSet implements api.HostSetStore.
func (s *JSONContractStore) SetHostSet(name string, hosts []consensus.PublicKey) error {
	s.EphemeralContractStore.SetHostSet(name, hosts)
	return s.save()
}

// NewJSONContractStore returns a new JSONContractStore.
func NewJSONContractStore(dir string) (*JSONContractStore, error) {
	s := &JSONContractStore{
		EphemeralContractStore: NewEphemeralContractStore(),
		dir:                    dir,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

type (
	// SQLContractStore implements the bus.ContractStore interface using SQL as the
	// persistence backend.
	SQLContractStore struct {
		db *gorm.DB
	}

	dbContractRHPv2 struct {
		ID         []byte                   `gorm:"primaryKey"`
		Revision   dbFileContractRevision   `gorm:"constraint:OnDelete:CASCADE;foreignKey:ParentID;references:ID"` //CASCADE to delete revision too
		Signatures []dbTransactionSignature `gorm:"constraint:OnDelete:CASCADE;foreignKey:ParentID;references:ID"` // CASCADE to delete signatures too
	}

	dbFileContractRevision struct {
		ParentID              []byte             `gorm:"primaryKey"` // only one revision for a given parent
		UnlockConditions      dbUnlockConditions `gorm:"constraint:OnDelete:CASCADE;foreignKey:ParentID;references:ParentID"`
		NewRevisionNumber     uint64
		NewFileSize           uint64
		NewFileMerkleRoot     []byte
		NewWindowStart        types.BlockHeight
		NewWindowEnd          types.BlockHeight
		NewValidProofOutputs  []dbSiacoinOutput `gorm:"constraint:OnDelete:CASCADE;foreignKey:ParentID;References:ParentID"` // CASCADE to delete output
		NewMissedProofOutputs []dbSiacoinOutput `gorm:"constraint:OnDelete:CASCADE;foreignKey:ParentID;References:ParentID"` // CASCADE to delete output
		NewUnlockHash         []byte
	}

	dbTransactionSignature struct {
		ID             uint64 `gorm:"primaryKey"`
		ParentID       []byte `gorm:"index"`
		PublicKeyIndex uint64
		Timelock       types.BlockHeight
		CoveredFields  []byte
		Signature      []byte
	}

	dbUnlockConditions struct {
		ParentID           []byte `gorm:"primaryKey"` // only one set of UnlockConditions for a given parent
		Timelock           types.BlockHeight
		PublicKeys         []dbSiaPublicKey `gorm:"constraint:OnDelete:CASCADE;foreignKey:UnlockConditionID;references:ParentID"` // CASCADE to delete pubkeys
		SignaturesRequired uint64
	}

	dbSiaPublicKey struct {
		ID                uint64 `gorm:"primaryKey"`
		Algorithm         []byte
		Key               []byte
		UnlockConditionID []byte `gorm:"index"`
	}

	dbSiacoinOutput struct {
		ID         uint64 `gorm:"primaryKey"`
		ParentID   []byte `gorm:"index"`
		UnlockHash []byte
		Value      string
	}
)

// TableName implements the gorm.Tabler interface.
func (dbContractRHPv2) TableName() string { return "contracts_v2" }

// TableName implements the gorm.Tabler interface.
func (dbFileContractRevision) TableName() string { return "file_contract_revisions" }

// TableName implements the gorm.Tabler interface.
func (dbTransactionSignature) TableName() string { return "transaction_signatures" }

// TableName implements the gorm.Tabler interface.
func (dbUnlockConditions) TableName() string { return "unlock_conditions" }

// TableName implements the gorm.Tabler interface.
func (dbSiaPublicKey) TableName() string { return "public_keys" }

// TableName implements the gorm.Tabler interface.
func (dbSiacoinOutput) TableName() string { return "siacoin_outputs" }

// NewSQLContractStore creates a new SQLContractStore from a given gorm
// Dialector.
func NewSQLContractStore(conn gorm.Dialector, migrate bool) (*SQLContractStore, error) {
	db, err := gorm.Open(conn, &gorm.Config{})
	if err != nil {
		return nil, err
	}

	if migrate {
		// Create the tables.
		tables := []interface{}{
			&dbContractRHPv2{},
			&dbFileContractRevision{},
			&dbTransactionSignature{},
			&dbUnlockConditions{},
			&dbSiaPublicKey{},
			&dbSiacoinOutput{},
		}
		if err := db.AutoMigrate(tables...); err != nil {
			return nil, err
		}
		if res := db.Exec("PRAGMA foreign_keys = ON", nil); res.Error != nil {
			return nil, res.Error
		}
	}

	return &SQLContractStore{
		db: db,
	}, nil
}

// AddContract implements the bus.ContractStore interface.
func (s *SQLContractStore) AddContract(c rhpv2.Contract) error {
	panic("not implemented")
}

// Contracts implements the bus.ContractStore interface.
func (s *SQLContractStore) Contracts() ([]rhpv2.Contract, error) {
	panic("not implemented")
}

// Contract implements the bus.ContractStore interface.
func (s *SQLContractStore) Contract(id types.FileContractID) (rhpv2.Contract, error) {
	panic("not implemented")
}

// RemoveContract implements the bus.ContractStore interface.
func (s *SQLContractStore) RemoveContract(id types.FileContractID) error {
	panic("not implemented")
}

// ContractsForDownload implements the worker.ContractStore interface.
func (s *SQLContractStore) ContractsForDownload(slab slab.Slab) ([]rhpv2.Contract, error) {
	panic("not implemented")
}

// ContractsForUpload implements the worker.ContractStore interface.
func (s *SQLContractStore) ContractsForUpload() ([]rhpv2.Contract, error) {
	panic("not implemented")
}
