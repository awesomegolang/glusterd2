package bricksplanner

import (
	"errors"
	"fmt"
	"path"
	"strconv"

	"github.com/gluster/glusterd2/glusterd2/volume"
	"github.com/gluster/glusterd2/pkg/api"
	"github.com/gluster/glusterd2/pkg/lvmutils"
	gutils "github.com/gluster/glusterd2/pkg/utils"

	config "github.com/spf13/viper"
)

const (
	minBrickSize            = 20 * gutils.MiB
	defaultMaxLoopBrickSize = 100 * gutils.GiB
)

func handleReplicaSubvolReq(req *api.VolCreateReq) error {
	if req.ReplicaCount < 2 {
		return nil
	}

	if req.ReplicaCount > 3 {
		return errors.New("invalid Replica Count")
	}
	req.SubvolType = "replicate"
	if req.ArbiterCount > 1 {
		return errors.New("invalid Arbiter Count")
	}

	return nil
}

func handleDisperseSubvolReq(req *api.VolCreateReq) error {
	if req.DisperseCount == 0 && req.DisperseDataCount == 0 && req.DisperseRedundancyCount == 0 {
		return nil
	}

	req.SubvolType = "disperse"

	if req.DisperseDataCount > 0 && req.DisperseRedundancyCount <= 0 {
		return errors.New("disperse redundancy count is required")
	}

	if req.DisperseDataCount > 0 {
		req.DisperseCount = req.DisperseDataCount + req.DisperseRedundancyCount
	}

	// Calculate Redundancy Value
	if req.DisperseRedundancyCount <= 0 {
		req.DisperseRedundancyCount = volume.GetRedundancy(uint(req.DisperseCount))
	}

	if req.DisperseDataCount <= 0 {
		req.DisperseDataCount = req.DisperseCount - req.DisperseRedundancyCount
	}

	if 2*req.DisperseRedundancyCount >= req.DisperseCount {
		return errors.New("invalid redundancy count")
	}

	return nil
}

// Based on the provided values like replica count, distribute count etc,
// brick layout will be created. Peer and device information for bricks are
// not available with the layout
func getBricksLayout(req *api.VolCreateReq) ([]api.SubvolReq, error) {
	var err error
	bricksMountRoot := path.Join(config.GetString("rundir"), "/bricks")

	// Default Subvol Type
	req.SubvolType = "distribute"

	// Validations if replica and arbiter sub volume
	err = handleReplicaSubvolReq(req)
	if err != nil {
		return nil, err
	}

	// Validations if disperse sub volume
	err = handleDisperseSubvolReq(req)
	if err != nil {
		return nil, err
	}

	if req.MaxBrickSize > 0 && req.MaxBrickSize < minBrickSize {
		return nil, errors.New("invalid max-brick-size, Minimum size required is " + strconv.Itoa(minBrickSize))
	}

	// Limit max loopback brick size to 100GiB
	if req.ProvisionerType == api.ProvisionerTypeLoop {
		if req.MaxBrickSize > defaultMaxLoopBrickSize {
			return nil, errors.New("invalid max-brick-size, max brick size supported for loop back bricks is " + strconv.Itoa(defaultMaxLoopBrickSize))
		}

		// If max brick size is not set
		if req.MaxBrickSize == 0 {
			req.MaxBrickSize = defaultMaxLoopBrickSize
		}
	}

	// If max Brick size is specified then decide distribute
	// count and Volume Size based on Volume Type
	if req.MaxBrickSize > 0 && req.Size > req.MaxBrickSize {
		// In case of replica and distribute, brick size is equal to
		// subvolume size, In case of disperse volume
		// subvol size = brick size * disperse-data-count
		maxSubvolSize := req.MaxBrickSize
		if req.DisperseDataCount > 0 {
			maxSubvolSize = req.MaxBrickSize * uint64(req.DisperseDataCount)
		}
		req.DistributeCount = int(req.Size / maxSubvolSize)
		if req.Size%maxSubvolSize > 0 {
			req.DistributeCount++
		}
	}

	numSubvols := 1
	if req.DistributeCount > 0 {
		numSubvols = req.DistributeCount
	}

	// User input will be in Bytes
	subvolSize := req.Size
	if numSubvols > 1 {
		subvolSize = subvolSize / uint64(numSubvols)
	}

	subvolplanner, exists := subvolPlanners[req.SubvolType]
	if !exists {
		return nil, errors.New("subvolume type not supported")
	}

	// Initialize the planner
	subvolplanner.Init(req, subvolSize)

	var subvols []api.SubvolReq

	// Create a Bricks layout based on replica count and
	// other details. Brick Path, PeerID information will
	// be added later.
	for i := 0; i < numSubvols; i++ {
		var bricks []api.BrickReq
		for j := 0; j < subvolplanner.BricksCount(); j++ {
			eachBrickSize := subvolplanner.BrickSize(j)
			brickType := subvolplanner.BrickType(j)
			if eachBrickSize < minBrickSize {
				return nil, errors.New("brick size is too small")
			}
			eachBrickTpSize := uint64(float64(eachBrickSize) * req.SnapshotReserveFactor)

			mntopts := "rw,inode64,noatime,nouuid,discard"
			if req.ProvisionerType == api.ProvisionerTypeLoop {
				mntopts += ",loop"
			}

			tpsize := lvmutils.NormalizeSize(eachBrickTpSize)
			tpmsize := lvmutils.GetPoolMetadataSize(eachBrickTpSize)
			bricks = append(bricks, api.BrickReq{
				Type:           brickType,
				Path:           fmt.Sprintf("%s/%s/subvol%d/brick%d/brick", bricksMountRoot, req.Name, i+1, j+1),
				BrickDirSuffix: "/brick",
				TpName:         fmt.Sprintf("tp_%s_s%d_b%d", req.Name, i+1, j+1),
				LvName:         fmt.Sprintf("brick_%s_s%d_b%d", req.Name, i+1, j+1),
				Size:           lvmutils.NormalizeSize(eachBrickSize),
				TpSize:         tpsize,
				TpMetadataSize: tpmsize,
				TotalSize:      tpsize + tpmsize,
				FsType:         "xfs",
				MntOpts:        mntopts,
			})
		}

		subvols = append(subvols, api.SubvolReq{
			Type:          req.SubvolType,
			Bricks:        bricks,
			ReplicaCount:  req.ReplicaCount,
			ArbiterCount:  req.ArbiterCount,
			DisperseCount: req.DisperseCount,
		})
	}

	return subvols, nil
}

// PlanBricks creates the brick layout with chosen device and size information
func PlanBricks(req *api.VolCreateReq) error {
	availableVgs, err := GetAvailableVgs(req)
	if err != nil {
		return err
	}

	if len(availableVgs) == 0 {
		return errors.New("no devices registered or available for allocating bricks")
	}

	subvols, err := getBricksLayout(req)
	if err != nil {
		return err
	}

	zones := make(map[string]struct{})

	for idx, sv := range subvols {
		// If zones overlap is not specified then do not
		// reset the zones map so that other subvol bricks
		// will not get allocated in the same zones
		if req.SubvolZonesOverlap {
			zones = make(map[string]struct{})
		}

		// For the list of bricks, first try to utilize all the
		// unutilized devices, Once all the devices are used, then try
		// with device with expected space available.
		numBricksAllocated := 0
		for bidx, b := range sv.Bricks {
			for _, vg := range availableVgs {
				_, zoneUsed := zones[vg.Zone]
				if vg.AvailableSize >= b.TotalSize && !zoneUsed && !vg.Used {
					subvols[idx].Bricks[bidx].PeerID = vg.PeerID
					subvols[idx].Bricks[bidx].VgName = vg.Name
					subvols[idx].Bricks[bidx].RootDevice = vg.Device
					subvols[idx].Bricks[bidx].DevicePath = "/dev/" + vg.Name + "/" + b.LvName
					if req.ProvisionerType == api.ProvisionerTypeLoop {
						subvols[idx].Bricks[bidx].DevicePath = vg.Device + "/" + b.TpName + "/" + b.LvName + ".img"
					}

					zones[vg.Zone] = struct{}{}
					numBricksAllocated++
					vg.AvailableSize -= b.TotalSize
					vg.Used = true
					break
				}
			}
		}

		// All bricks allocation not satisfied since only fresh devices are
		// considered. Now consider all devices with available space
		if len(sv.Bricks) == numBricksAllocated {
			continue
		}

		// Try allocating for remaining bricks, No fresh device is available
		// but enough space is available in the devices
		for bidx := numBricksAllocated; bidx < len(sv.Bricks); bidx++ {
			b := sv.Bricks[bidx]
			for _, vg := range availableVgs {
				_, zoneUsed := zones[vg.Zone]
				if vg.AvailableSize >= b.TotalSize && !zoneUsed {
					subvols[idx].Bricks[bidx].PeerID = vg.PeerID
					subvols[idx].Bricks[bidx].VgName = vg.Name
					subvols[idx].Bricks[bidx].RootDevice = vg.Device
					subvols[idx].Bricks[bidx].DevicePath = "/dev/" + vg.Name + "/" + b.LvName
					if req.ProvisionerType == api.ProvisionerTypeLoop {
						subvols[idx].Bricks[bidx].DevicePath = vg.Device + "/" + b.TpName + "/" + b.LvName + ".img"
					}

					zones[vg.Zone] = struct{}{}
					numBricksAllocated++
					vg.AvailableSize -= b.TotalSize
					vg.Used = true
					break
				}
			}
		}

		// If the devices are not available as it is required for Volume.
		if len(sv.Bricks) != numBricksAllocated {
			return errors.New("no space available or all the devices are not registered")
		}
	}

	req.Subvols = subvols
	return nil
}
