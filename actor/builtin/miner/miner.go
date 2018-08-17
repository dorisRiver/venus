package miner

import (
	"math/big"

	cbor "gx/ipfs/QmPbqRavwDZLfmpeW6eoyAoQ5rT2LoCW98JhvRc22CqkZS/go-ipld-cbor"
	"gx/ipfs/QmdVrMn1LhB4ybb8hMVaMLXnA8XRSewMnK6YqXKXoTcRvN/go-libp2p-peer"

	"github.com/filecoin-project/go-filecoin/abi"
	"github.com/filecoin-project/go-filecoin/actor"
	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/exec"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/vm/errors"
)

func init() {
	cbor.RegisterCborType(State{})
	cbor.RegisterCborType(Sector{})
}

// MaximumPublicKeySize is a limit on how big a public key can be.
const MaximumPublicKeySize = 100

const (
	// ErrPublicKeyTooBig indicates an invalid public key.
	ErrPublicKeyTooBig = 33
	// ErrInvalidSector indicates and invalid sector id.
	ErrInvalidSector = 34
	// ErrSectorCommited indicates the sector has already been committed.
	ErrSectorCommited = 35
	// ErrStoragemarketCallFailed indicates the call to commit the deal failed.
	ErrStoragemarketCallFailed = 36
	// ErrCallerUnauthorized signals an unauthorized caller.
	ErrCallerUnauthorized = 37
	// ErrInsufficientPledge signals insufficient pledge for what you are trying to do.
	ErrInsufficientPledge = 38
)

// Errors map error codes to revert errors this actor may return.
var Errors = map[uint8]error{
	ErrPublicKeyTooBig:         errors.NewCodedRevertErrorf(ErrPublicKeyTooBig, "public key must be less than %d bytes", MaximumPublicKeySize),
	ErrInvalidSector:           errors.NewCodedRevertErrorf(ErrInvalidSector, "sectorID out of range"),
	ErrSectorCommited:          errors.NewCodedRevertErrorf(ErrSectorCommited, "sector already committed"),
	ErrStoragemarketCallFailed: errors.NewCodedRevertErrorf(ErrStoragemarketCallFailed, "call to StorageMarket failed"),
	ErrCallerUnauthorized:      errors.NewCodedRevertErrorf(ErrCallerUnauthorized, "not authorized to call the method"),
	ErrInsufficientPledge:      errors.NewCodedRevertErrorf(ErrInsufficientPledge, "not enough pledged"),
}

// Actor is the miner actor.
type Actor struct{}

// Sector is the on-chain representation of a sector.
type Sector struct {
	CommR []byte
	Deals []uint64
}

// State is the miner actors storage.
type State struct {
	Owner types.Address

	// PeerID references the libp2p identity that the miner is operating.
	PeerID peer.ID

	// PublicKey is used to validate blocks generated by the miner this actor represents.
	PublicKey []byte

	// Pledge is amount the space being offered up by this miner.
	// TODO: maybe minimum granularity is more than 1 byte?
	PledgeBytes *types.BytesAmount

	// Collateral is the total amount of filecoin being held as collateral for
	// the miners pledge.
	Collateral *types.AttoFIL

	Sectors []*Sector

	LockedStorage *types.BytesAmount // LockedStorage is the amount of the miner's storage that is used.
	Power         *types.BytesAmount
}

// NewActor returns a new miner actor
func NewActor() *types.Actor {
	return types.NewActor(types.MinerActorCodeCid, types.NewZeroAttoFIL())
}

// NewState creates a miner state struct
func NewState(owner types.Address, key []byte, pledge *types.BytesAmount, pid peer.ID, collateral *types.AttoFIL) *State {
	return &State{
		Owner:         owner,
		PeerID:        pid,
		PublicKey:     key,
		PledgeBytes:   pledge,
		Collateral:    collateral,
		LockedStorage: types.NewBytesAmount(0),
	}
}

// InitializeState stores this miner's initial data structure.
func (ma *Actor) InitializeState(storage exec.Storage, initializerData interface{}) error {
	minerState, ok := initializerData.(*State)
	if !ok {
		return errors.NewFaultError("Initial state to miner actor is not a miner.State struct")
	}

	// TODO: we should validate this is actually a public key (possibly the owner's public key) once we have a better
	// TODO: idea what crypto looks like.
	if len(minerState.PublicKey) > MaximumPublicKeySize {
		return Errors[ErrPublicKeyTooBig]
	}

	stateBytes, err := cbor.DumpObject(minerState)
	if err != nil {
		return err
	}

	id, err := storage.Put(stateBytes)
	if err != nil {
		return err
	}

	return storage.Commit(id, nil)
}

var _ exec.ExecutableActor = (*Actor)(nil)

var minerExports = exec.Exports{
	"addAsk": &exec.FunctionSignature{
		Params: []abi.Type{abi.AttoFIL, abi.BytesAmount},
		Return: []abi.Type{abi.Integer},
	},
	"getOwner": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.Address},
	},
	"addDealsToSector": &exec.FunctionSignature{
		Params: []abi.Type{abi.Integer, abi.UintArray},
		Return: []abi.Type{abi.Integer},
	},
	"commitSector": &exec.FunctionSignature{
		Params: []abi.Type{abi.Integer, abi.Bytes, abi.UintArray},
		Return: []abi.Type{abi.Integer},
	},
	"getKey": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.Bytes},
	},
	"getPeerID": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.PeerID},
	},
	"updatePeerID": &exec.FunctionSignature{
		Params: []abi.Type{abi.PeerID},
		Return: []abi.Type{},
	},
}

// Exports returns the miner actors exported functions.
func (ma *Actor) Exports() exec.Exports {
	return minerExports
}

// AddAsk adds an ask via this miner to the storage markets orderbook.
func (ma *Actor) AddAsk(ctx exec.VMContext, price *types.AttoFIL, size *types.BytesAmount) (*big.Int, uint8,
	error) {
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		if ctx.Message().From != state.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		// compute locked storage + new ask
		total := state.LockedStorage.Add(size)

		if total.GreaterThan(state.PledgeBytes) {
			return nil, Errors[ErrInsufficientPledge]
		}

		state.LockedStorage = total

		// TODO: kinda feels weird that I can't get a real type back here
		out, ret, err := ctx.Send(address.StorageMarketAddress, "addAsk", nil, []interface{}{price, size})
		if err != nil {
			return nil, err
		}

		askID, err := abi.Deserialize(out[0], abi.Integer)
		if err != nil {
			return nil, errors.FaultErrorWrap(err, "error deserializing")
		}

		if ret != 0 {
			return nil, Errors[ErrStoragemarketCallFailed]
		}

		return askID.Val, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	askID, ok := out.(*big.Int)
	if !ok {
		return nil, 1, errors.NewRevertErrorf("expected an Integer return value from call, but got %T instead", out)
	}

	return askID, 0, nil
}

// GetOwner returns the miners owner.
func (ma *Actor) GetOwner(ctx exec.VMContext) (types.Address, uint8, error) {
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.Owner, nil
	})
	if err != nil {
		return types.Address{}, errors.CodeError(err), err
	}

	a, ok := out.(types.Address)
	if !ok {
		return types.Address{}, 1, errors.NewFaultErrorf("expected an Address return value from call, but got %T instead", out)
	}

	return a, 0, nil
}

// AddDealsToSector adds deals to a sector. If the sectorID given is -1, a new
// sector ID is allocated. The sector ID that deals are added to is returned.
func (ma *Actor) AddDealsToSector(ctx exec.VMContext, sectorID int64, deals []uint64) (*big.Int, uint8,
	error) {
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.upsertDealsToSector(sectorID, deals)
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	secIDout, ok := out.(int64)
	if !ok {
		return nil, 1, errors.NewRevertError("expected an int64")
	}

	return big.NewInt(secIDout), 0, nil
}

func (state *State) upsertDealsToSector(sectorID int64, deals []uint64) (int64, error) {
	if sectorID == int64(len(state.Sectors)) {
		state.Sectors = append(state.Sectors, new(Sector))
	}
	if sectorID > int64(len(state.Sectors)) {
		return 0, Errors[ErrInvalidSector]
	}
	sector := state.Sectors[sectorID]
	if sector.CommR != nil {
		return 0, Errors[ErrSectorCommited]
	}

	sector.Deals = append(sector.Deals, deals...)
	return sectorID, nil
}

// CommitSector adds a commitment to the specified sector
// if sectorID is -1, a new sector will be allocated.
// if passing an existing sector ID, any deals given here will be added to the
// deals already added to that sector.
func (ma *Actor) CommitSector(ctx exec.VMContext, sectorID *big.Int, commR []byte, deals []uint64) (*big.Int, uint8, error) {
	var state State

	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		sectorIDInt := sectorID.Int64()
		if len(deals) != 0 {
			sid, err := state.upsertDealsToSector(sectorIDInt, deals)
			if err != nil {
				return nil, err
			}
			sectorIDInt = sid
		}

		sector := state.Sectors[sectorIDInt]
		if sector.CommR != nil {
			return nil, Errors[ErrSectorCommited]
		}

		resp, ret, err := ctx.Send(address.StorageMarketAddress, "commitDeals", nil, []interface{}{sector.Deals})
		if err != nil {
			return nil, err
		}
		if ret != 0 {
			return nil, Errors[ErrStoragemarketCallFailed]
		}

		sector.CommR = commR
		power := types.NewBytesAmountFromBytes(resp[0])
		state.Power = state.Power.Add(power)

		return sectorID, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	secIDout, ok := out.(*big.Int)
	if !ok {
		return nil, 1, errors.NewRevertError("expected a big.Int")
	}

	return secIDout, 0, nil
}

// GetKey returns the public key for this miner.
func (ma *Actor) GetKey(ctx exec.VMContext) ([]byte, uint8, error) {
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.PublicKey, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	validOut, ok := out.([]byte)
	if !ok {
		return nil, 1, errors.NewRevertError("expected a byte slice")
	}

	return validOut, 0, nil
}

// GetPeerID returns the libp2p peer ID that this miner can be reached at.
func (ma *Actor) GetPeerID(ctx exec.VMContext) (peer.ID, uint8, error) {
	var state State

	chunk, err := ctx.ReadStorage()
	if err != nil {
		return peer.ID(""), errors.CodeError(err), err
	}

	if err := actor.UnmarshalStorage(chunk, &state); err != nil {
		return peer.ID(""), errors.CodeError(err), err
	}

	return state.PeerID, 0, nil
}

// UpdatePeerID is used to update the peerID this miner is operating under.
func (ma *Actor) UpdatePeerID(ctx exec.VMContext, pid peer.ID) (uint8, error) {
	var storage State
	_, err := actor.WithState(ctx, &storage, func() (interface{}, error) {
		// verify that the caller is authorized to perform update
		if ctx.Message().From != storage.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		storage.PeerID = pid

		return nil, nil
	})
	if err != nil {
		return errors.CodeError(err), err
	}

	return 0, nil
}
