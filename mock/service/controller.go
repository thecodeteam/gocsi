package service

import (
	"fmt"
	"math"
	"path"
	"strconv"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func (s *service) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {

	// Check to see if the volume already exists.
	if i, v := s.findVolByName(ctx, req.Name); i >= 0 {
		return &csi.CreateVolumeResponse{Volume: &v}, nil
	}

	// If no capacity is specified then use 100GiB
	capacity := gib100
	if cr := req.CapacityRange; cr != nil {
		if rb := cr.RequiredBytes; rb > 0 {
			capacity = rb
		}
		if lb := cr.LimitBytes; lb > 0 {
			capacity = lb
		}
	}

	// Create the volume and add it to the service's in-mem volume slice.
	v := s.newVolume(req.Name, capacity)
	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()
	s.vols = append(s.vols, v)

	return &csi.CreateVolumeResponse{Volume: &v}, nil
}

func (s *service) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {

	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()

	// If the volume does not exist then return an idempotent response.
	i, _ := s.findVolNoLock("id", req.VolumeId)
	if i < 0 {
		return &csi.DeleteVolumeResponse{}, nil
	}

	// This delete logic preserves order and prevents potential memory
	// leaks. The slice's elements may not be pointers, but the structs
	// themselves have fields that are.
	copy(s.vols[i:], s.vols[i+1:])
	s.vols[len(s.vols)-1] = csi.Volume{}
	s.vols = s.vols[:len(s.vols)-1]
	log.WithField("volumeID", req.VolumeId).Debug("mock delete volume")
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *service) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {

	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()

	i, v := s.findVolNoLock("id", req.VolumeId)
	if i < 0 {
		return nil, status.Error(codes.NotFound, req.VolumeId)
	}

	// devPathKey is the key in the volume's attributes that is set to a
	// mock device path if the volume has been published by the controller
	// to the specified node.
	devPathKey := path.Join(req.NodeId, "dev")

	// Check to see if the volume is already published.
	if device := v.Attributes[devPathKey]; device != "" {
		return &csi.ControllerPublishVolumeResponse{
			PublishInfo: map[string]string{
				"device": device,
			},
		}, nil
	}

	// Publish the volume.
	device := "/dev/mock"
	v.Attributes[devPathKey] = device
	s.vols[i] = v

	return &csi.ControllerPublishVolumeResponse{
		PublishInfo: map[string]string{
			"device": device,
		},
	}, nil
}

func (s *service) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {

	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()

	i, v := s.findVolNoLock("id", req.VolumeId)
	if i < 0 {
		return nil, status.Error(codes.NotFound, req.VolumeId)
	}

	// devPathKey is the key in the volume's attributes that is set to a
	// mock device path if the volume has been published by the controller
	// to the specified node.
	devPathKey := path.Join(req.NodeId, "dev")

	// Check to see if the volume is already unpublished.
	if v.Attributes[devPathKey] == "" {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Unpublish the volume.
	delete(v.Attributes, devPathKey)
	s.vols[i] = v

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *service) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {

	return &csi.ValidateVolumeCapabilitiesResponse{
		Supported: true,
	}, nil
}

func (s *service) ListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {

	// Copy the mock volumes into a new slice in order to avoid
	// locking the service's volume slice for the duration of the
	// ListVolumes RPC.
	var vols []csi.Volume
	func() {
		s.volsRWL.RLock()
		defer s.volsRWL.RUnlock()
		vols = make([]csi.Volume, len(s.vols))
		copy(vols, s.vols)
	}()

	var (
		ulenVols      = int32(len(vols))
		maxEntries    = req.MaxEntries
		startingToken int32
	)

	if v := req.StartingToken; v != "" {
		i, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"startingToken=%d !< int32=%d",
				startingToken, math.MaxUint32)
		}
		startingToken = int32(i)
	}

	if startingToken > ulenVols {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"startingToken=%d > len(vols)=%d",
			startingToken, ulenVols)
	}

	// Discern the number of remaining entries.
	rem := ulenVols - startingToken

	// If maxEntries is 0 or greater than the number of remaining entries then
	// set maxEntries to the number of remaining entries.
	if maxEntries == 0 || maxEntries > rem {
		maxEntries = rem
	}

	var (
		i       int
		j       = startingToken
		entries = make(
			[]*csi.ListVolumesResponse_Entry,
			maxEntries)
	)

	for i = 0; i < len(entries); i++ {
		entries[i] = &csi.ListVolumesResponse_Entry{
			Volume: &vols[j],
		}
		j++
	}

	var nextToken string
	if n := startingToken + int32(i); n < ulenVols {
		nextToken = fmt.Sprintf("%d", n)
	}

	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (s *service) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {

	return &csi.GetCapacityResponse{
		AvailableCapacity: tib100,
	}, nil
}

func (s *service) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_GET_CAPACITY,
					},
				},
			},
		},
	}, nil
}

func (s *service) ControllerProbe(
	ctx context.Context,
	req *csi.ControllerProbeRequest) (
	*csi.ControllerProbeResponse, error) {

	return &csi.ControllerProbeResponse{}, nil
}
