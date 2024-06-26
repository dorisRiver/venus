package beacon

import (
	"context"
	"fmt"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/filecoin-project/venus/venus-shared/types"
	logging "github.com/ipfs/go-log"
)

var log = logging.Logger("beacon")

type Response struct {
	Entry types.BeaconEntry
	Err   error
}

type BeaconPoint struct { //nolint
	Start  abi.ChainEpoch
	Beacon RandomBeacon
}

// RandomBeacon represents a system that provides randomness to Lotus.
// Other components interrogate the RandomBeacon to acquire randomness that's
// valid for a specific chain epoch. Also to verify beacon entries that have
// been posted on chain.
type RandomBeacon interface {
	Entry(context.Context, uint64) <-chan Response
	// VerifyEntry(types.BeaconEntry, types.BeaconEntry) error
	VerifyEntry(entry types.BeaconEntry, prevEntrySig []byte) error
	MaxBeaconRoundForEpoch(network.Version, abi.ChainEpoch) uint64
	IsChained() bool
}

// ValidateBlockValues Verify that the beacon in the block header is correct, first get beacon server at block epoch and parent block epoch in schedule.
// if paraent beacon is the same beacon server. value beacon normally but if not equal, means that the pre entry in another beacon chain, so just validate
// beacon value in current block header. the first values is parent beacon the second value is current beacon.
func ValidateBlockValues(bSchedule Schedule, nv network.Version, h *types.BlockHeader, parentEpoch abi.ChainEpoch, prevEntry *types.BeaconEntry) error {
	parentBeacon := bSchedule.BeaconForEpoch(parentEpoch)
	currBeacon := bSchedule.BeaconForEpoch(h.Height)
	// When we have "chained" beacons, two entries at a fork are required.
	if parentBeacon != currBeacon && currBeacon.IsChained() {
		if len(h.BeaconEntries) != 2 {
			return fmt.Errorf("expected two beacon entries at beacon fork, got %d", len(h.BeaconEntries))
		}
		err := currBeacon.VerifyEntry(h.BeaconEntries[1], h.BeaconEntries[0].Data)
		if err != nil {
			return fmt.Errorf("beacon at fork point invalid: (%v, %v): %w",
				h.BeaconEntries[1], h.BeaconEntries[0], err)
		}
		return nil
	}

	maxRound := currBeacon.MaxBeaconRoundForEpoch(nv, h.Height)

	// We don't expect to ever actually meet this condition
	if maxRound == prevEntry.Round {
		if len(h.BeaconEntries) != 0 {
			return fmt.Errorf("expected not to have any beacon entries in this block, got %d", len(h.BeaconEntries))
		}
		return nil
	}

	if len(h.BeaconEntries) == 0 {
		return fmt.Errorf("expected to have beacon entries in this block, but didn't find any")
	}

	// We skip verifying the genesis entry when randomness is "chained".
	if currBeacon.IsChained() && prevEntry.Round == 0 {
		return nil
	}

	last := h.BeaconEntries[len(h.BeaconEntries)-1]
	if last.Round != maxRound {
		return fmt.Errorf("expected final beacon entry in block to be at round %d, got %d", maxRound, last.Round)
	}

	// If the beacon is UNchained, verify that the block only includes the rounds we want for the epochs in between parentEpoch and h.Height
	// For chained beacons, you must have all the rounds forming a valid chain with prevEntry, so we can skip this step
	if !currBeacon.IsChained() {
		// Verify that all other entries' rounds are as expected for the epochs in between parentEpoch and h.Height
		for i, e := range h.BeaconEntries {
			correctRound := currBeacon.MaxBeaconRoundForEpoch(nv, parentEpoch+abi.ChainEpoch(i)+1)
			if e.Round != correctRound {
				return fmt.Errorf("unexpected beacon round %d, expected %d for epoch %d", e.Round, correctRound, parentEpoch+abi.ChainEpoch(i))
			}
		}
	}

	// Verify the beacon entries themselves
	for i, e := range h.BeaconEntries {
		if err := currBeacon.VerifyEntry(e, prevEntry.Data); err != nil {
			return fmt.Errorf("beacon entry %d (%d - %x (%d)) was invalid: %w", i, e.Round, e.Data, len(e.Data), err)
		}
		prevEntry = &h.BeaconEntries[i]
	}

	return nil
}

func BeaconEntriesForBlock(ctx context.Context, bSchedule Schedule, nv network.Version, epoch abi.ChainEpoch, parentEpoch abi.ChainEpoch, prev types.BeaconEntry) ([]types.BeaconEntry, error) { //nolint
	// When we have "chained" beacons, two entries at a fork are required.
	parentBeacon := bSchedule.BeaconForEpoch(parentEpoch)
	currBeacon := bSchedule.BeaconForEpoch(epoch)
	if parentBeacon != currBeacon && currBeacon.IsChained() {
		// Fork logic
		round := currBeacon.MaxBeaconRoundForEpoch(nv, epoch)
		out := make([]types.BeaconEntry, 2)
		rch := currBeacon.Entry(ctx, round-1)
		res := <-rch
		if res.Err != nil {
			return nil, fmt.Errorf("getting entry %d returned error: %w", round-1, res.Err)
		}
		out[0] = res.Entry
		rch = currBeacon.Entry(ctx, round)
		res = <-rch
		if res.Err != nil {
			return nil, fmt.Errorf("getting entry %d returned error: %w", round, res.Err)
		}
		out[1] = res.Entry
		return out, nil
	}

	start := time.Now()

	maxRound := currBeacon.MaxBeaconRoundForEpoch(nv, epoch)
	// We don't expect this to ever be the case
	if maxRound == prev.Round {
		return nil, nil
	}

	// TODO: this is a sketchy way to handle the genesis block not having a beacon entry
	if prev.Round == 0 {
		prev.Round = maxRound - 1
	}

	var out []types.BeaconEntry
	for currEpoch := epoch; currEpoch > parentEpoch; currEpoch-- {
		currRound := currBeacon.MaxBeaconRoundForEpoch(nv, currEpoch)
		rch := currBeacon.Entry(ctx, currRound)
		select {
		case resp := <-rch:
			if resp.Err != nil {
				return nil, fmt.Errorf("beacon entry request returned error: %w", resp.Err)
			}

			out = append(out, resp.Entry)
		case <-ctx.Done():
			return nil, fmt.Errorf("context timed out waiting on beacon entry to come back for epoch %d: %w", epoch, ctx.Err())
		}
	}

	log.Debugw("fetching beacon entries", "took", time.Since(start), "numEntries", len(out))
	reverse(out)
	return out, nil
}

func reverse(arr []types.BeaconEntry) {
	for i := 0; i < len(arr)/2; i++ {
		arr[i], arr[len(arr)-(1+i)] = arr[len(arr)-(1+i)], arr[i]
	}
}
