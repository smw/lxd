package main

import (
	"fmt"

	"github.com/gorilla/websocket"

	"github.com/lxc/lxd/lxd/types"
	"github.com/lxc/lxd/shared"
)

type MigrationStorageSourceDriver interface {
	/* snapshots for this container, if any */
	Snapshots() []container

	/* send any bits of the container/snapshots that are possible while the
	 * container is still running.
	 */
	SendWhileRunning(conn *websocket.Conn, op *operation) error

	/* send the final bits (e.g. a final delta snapshot for zfs, btrfs, or
	 * do a final rsync) of the fs after the container has been
	 * checkpointed. This will only be called when a container is actually
	 * being live migrated.
	 */
	SendAfterCheckpoint(conn *websocket.Conn) error

	/* Called after either success or failure of a migration, can be used
	 * to clean up any temporary snapshots, etc.
	 */
	Cleanup()
}

type rsyncStorageSourceDriver struct {
	container container
	snapshots []container
}

func (s rsyncStorageSourceDriver) Snapshots() []container {
	return s.snapshots
}

func (s rsyncStorageSourceDriver) SendWhileRunning(conn *websocket.Conn, op *operation) error {
	for _, send := range s.snapshots {
		if err := send.StorageStart(); err != nil {
			return err
		}
		defer send.StorageStop()

		path := send.Path()
		wrapper := StorageProgressReader(op, "fs_progress", send.Name())
		if err := RsyncSend(shared.AddSlash(path), conn, wrapper); err != nil {
			return err
		}
	}

	wrapper := StorageProgressReader(op, "fs_progress", s.container.Name())
	return RsyncSend(shared.AddSlash(s.container.Path()), conn, wrapper)
}

func (s rsyncStorageSourceDriver) SendAfterCheckpoint(conn *websocket.Conn) error {
	/* resync anything that changed between our first send and the checkpoint */
	return RsyncSend(shared.AddSlash(s.container.Path()), conn, nil)
}

func (s rsyncStorageSourceDriver) Cleanup() {
	/* no-op */
}

func rsyncMigrationSource(container container) (MigrationStorageSourceDriver, error) {
	snapshots, err := container.Snapshots()
	if err != nil {
		return nil, err
	}

	return rsyncStorageSourceDriver{container, snapshots}, nil
}

func snapshotProtobufToContainerArgs(containerName string, snap *Snapshot) containerArgs {
	config := map[string]string{}

	for _, ent := range snap.LocalConfig {
		config[ent.GetKey()] = ent.GetValue()
	}

	devices := types.Devices{}
	for _, ent := range snap.LocalDevices {
		props := map[string]string{}
		for _, prop := range ent.Config {
			props[prop.GetKey()] = prop.GetValue()
		}

		devices[ent.GetName()] = props
	}

	name := containerName + shared.SnapshotDelimiter + snap.GetName()
	return containerArgs{
		Name:         name,
		Ctype:        cTypeSnapshot,
		Config:       config,
		Profiles:     snap.Profiles,
		Ephemeral:    snap.GetEphemeral(),
		Devices:      devices,
		Architecture: int(snap.GetArchitecture()),
		Stateful:     snap.GetStateful(),
	}
}

func rsyncMigrationSink(live bool, container container, snapshots []*Snapshot, conn *websocket.Conn, srcIdmap *shared.IdmapSet, op *operation) error {
	if err := container.StorageStart(); err != nil {
		return err
	}
	defer container.StorageStop()

	// At this point we have already figured out the parent
	// container's root disk device so we can simply
	// retrieve it from the expanded devices.
	parentStoragePool := ""
	parentExpandedDevices := container.ExpandedDevices()
	parentLocalRootDiskDeviceKey, parentLocalRootDiskDevice, _ := containerGetRootDiskDevice(parentExpandedDevices)
	if parentLocalRootDiskDeviceKey != "" {
		parentStoragePool = parentLocalRootDiskDevice["pool"]
	}

	// A little neuroticism.
	if parentStoragePool == "" {
		return fmt.Errorf("The container's root device is missing the pool property.")
	}

	isDirBackend := container.Storage().GetStorageType() == storageTypeDir
	if isDirBackend {
		for _, snap := range snapshots {
			args := snapshotProtobufToContainerArgs(container.Name(), snap)

			// Ensure that snapshot and parent container have the
			// same storage pool in their local root disk device.
			// If the root disk device for the snapshot comes from a
			// profile on the new instance as well we don't need to
			// do anything.
			if args.Devices != nil {
				snapLocalRootDiskDeviceKey, _, _ := containerGetRootDiskDevice(args.Devices)
				if snapLocalRootDiskDeviceKey != "" {
					args.Devices[snapLocalRootDiskDeviceKey]["pool"] = parentStoragePool
				}
			}

			s, err := containerCreateEmptySnapshot(container.Daemon(), args)
			if err != nil {
				return err
			}

			wrapper := StorageProgressWriter(op, "fs_progress", s.Name())
			if err := RsyncRecv(shared.AddSlash(s.Path()), conn, wrapper); err != nil {
				return err
			}

			if err := ShiftIfNecessary(container, srcIdmap); err != nil {
				return err
			}
		}

		wrapper := StorageProgressWriter(op, "fs_progress", container.Name())
		if err := RsyncRecv(shared.AddSlash(container.Path()), conn, wrapper); err != nil {
			return err
		}
	} else {
		for _, snap := range snapshots {
			args := snapshotProtobufToContainerArgs(container.Name(), snap)

			// Ensure that snapshot and parent container have the
			// same storage pool in their local root disk device.
			// If the root disk device for the snapshot comes from a
			// profile on the new instance as well we don't need to
			// do anything.
			if args.Devices != nil {
				snapLocalRootDiskDeviceKey, _, _ := containerGetRootDiskDevice(args.Devices)
				if snapLocalRootDiskDeviceKey != "" {
					args.Devices[snapLocalRootDiskDeviceKey]["pool"] = parentStoragePool
				}
			}

			wrapper := StorageProgressWriter(op, "fs_progress", snap.GetName())
			if err := RsyncRecv(shared.AddSlash(container.Path()), conn, wrapper); err != nil {
				return err
			}

			if err := ShiftIfNecessary(container, srcIdmap); err != nil {
				return err
			}

			_, err := containerCreateAsSnapshot(container.Daemon(), args, container)
			if err != nil {
				return err
			}
		}

		wrapper := StorageProgressWriter(op, "fs_progress", container.Name())
		if err := RsyncRecv(shared.AddSlash(container.Path()), conn, wrapper); err != nil {
			return err
		}
	}

	if live {
		/* now receive the final sync */
		wrapper := StorageProgressWriter(op, "fs_progress", container.Name())
		if err := RsyncRecv(shared.AddSlash(container.Path()), conn, wrapper); err != nil {
			return err
		}
	}

	if err := ShiftIfNecessary(container, srcIdmap); err != nil {
		return err
	}

	return nil
}
