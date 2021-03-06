// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"
	"sync"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/concurrency"
	"github.com/youtube/vitess/go/vt/mysqlctl"
	tm "github.com/youtube/vitess/go/vt/tabletmanager"
	"github.com/youtube/vitess/go/vt/topo"
)

// keyspace related methods for Wrangler

func (wr *Wrangler) lockKeyspace(keyspace string, actionNode *tm.ActionNode) (lockPath string, err error) {
	log.Infof("Locking keyspace %v for action %v", keyspace, actionNode.Action)
	return wr.ts.LockKeyspaceForAction(keyspace, tm.ActionNodeToJson(actionNode), wr.lockTimeout, interrupted)
}

func (wr *Wrangler) unlockKeyspace(keyspace string, actionNode *tm.ActionNode, lockPath string, actionError error) error {
	// first update the actionNode
	if actionError != nil {
		log.Infof("Unlocking keyspace %v for action %v with error %v", keyspace, actionNode.Action, actionError)
		actionNode.Error = actionError.Error()
		actionNode.State = tm.ACTION_STATE_FAILED
	} else {
		log.Infof("Unlocking keyspace %v for successful action %v", keyspace, actionNode.Action)
		actionNode.Error = ""
		actionNode.State = tm.ACTION_STATE_DONE
	}
	err := wr.ts.UnlockKeyspaceForAction(keyspace, lockPath, tm.ActionNodeToJson(actionNode))
	if actionError != nil {
		if err != nil {
			// this will be masked
			log.Warningf("UnlockKeyspaceForAction failed: %v", err)
		}
		return actionError
	}
	return err
}

func (wr *Wrangler) MigrateServedTypes(keyspace, shard string, servedType topo.TabletType, reverse bool) error {
	// we cannot migrate a master back, since when master migration
	// is done, the source shards are dead
	if reverse && servedType == topo.TYPE_MASTER {
		return fmt.Errorf("Cannot migrate master back to %v/%v", keyspace, shard)
	}

	// first figure out the destination shards
	// TODO(alainjobart) for now we only look in the same keyspace.
	// We might want to look elsewhere eventually too, maybe through
	// an extra command line parameter?
	shardNames, err := wr.ts.GetShardNames(keyspace)
	if err != nil {
		return nil
	}
	destinationShards := make([]*topo.ShardInfo, 0, 0)
	for _, shardName := range shardNames {
		si, err := wr.ts.GetShard(keyspace, shardName)
		if err != nil {
			return err
		}

		for _, sourceShard := range si.SourceShards {
			if sourceShard.Keyspace == keyspace && sourceShard.Shard == shard {
				// this shard is replicating from the source shard we specified
				log.Infof("Found %v/%v as a destination shard", si.Keyspace(), si.ShardName())
				destinationShards = append(destinationShards, si)
				break
			}
		}
	}
	if len(destinationShards) == 0 {
		return fmt.Errorf("Cannot find any destination shard replicating from %v/%v", keyspace, shard)
	}

	// TODO(alainjobart) for a reverse split, we also need to find
	// more sources. For now, a single source is all we need, but we
	// still use a list of sources to not have to change the code later.
	sourceShards := make([]*topo.ShardInfo, 0, 0)

	// Verify the source has the type we're migrating
	si, err := wr.ts.GetShard(keyspace, shard)
	if err != nil {
		return err
	}
	foundType := topo.IsTypeInList(servedType, si.ServedTypes)
	if reverse {
		if foundType {
			return fmt.Errorf("Source shard %v/%v is already serving type %v", keyspace, shard, servedType)
		}
	} else {
		if !foundType {
			return fmt.Errorf("Source shard %v/%v is not serving type %v", keyspace, shard, servedType)
		}
	}
	if servedType == topo.TYPE_MASTER && len(si.ServedTypes) > 1 {
		return fmt.Errorf("Cannot migrate master out of %v/%v until everything else is migrated out", keyspace, shard)
	}
	sourceShards = append(sourceShards, si)

	// lock the shards: sources, then destinations
	// (note they're all ordered by shard name)
	actionNode := wr.ai.MigrateServedTypes(servedType)
	sourceLockPath := make([]string, len(sourceShards))
	for i, si := range sourceShards {
		sourceLockPath[i], err = wr.lockShard(si.Keyspace(), si.ShardName(), actionNode)
		if err != nil {
			log.Errorf("Failed to lock source shard %v/%v, may need to unlock other shards manually", si.Keyspace(), si.ShardName())
			return err
		}
	}
	destinationLockPath := make([]string, len(destinationShards))
	for i, si := range destinationShards {
		destinationLockPath[i], err = wr.lockShard(si.Keyspace(), si.ShardName(), actionNode)
		if err != nil {
			log.Errorf("Failed to lock destination shard %v/%v, may need to unlock other shards manually", si.Keyspace(), si.ShardName())
			return err
		}
	}

	// record the action error and all unlock errors
	rec := concurrency.AllErrorRecorder{}

	// execute the migration
	rec.RecordError(wr.migrateServedTypes(sourceShards, destinationShards, servedType, reverse))

	// unlock the shards, we're done
	for i := len(destinationShards) - 1; i >= 0; i-- {
		rec.RecordError(wr.unlockShard(destinationShards[i].Keyspace(), destinationShards[i].ShardName(), actionNode, destinationLockPath[i], nil))
	}
	for i := len(sourceShards) - 1; i >= 0; i-- {
		rec.RecordError(wr.unlockShard(sourceShards[i].Keyspace(), sourceShards[i].ShardName(), actionNode, sourceLockPath[i], nil))
	}

	// rebuild the keyspace serving graph if there was no error
	if rec.Error() == nil {
		rec.RecordError(wr.RebuildKeyspaceGraph(keyspace, nil, true))
	}

	return rec.Error()
}

func removeType(tabletType topo.TabletType, types []topo.TabletType) ([]topo.TabletType, bool) {
	result := make([]topo.TabletType, 0, len(types)-1)
	found := false
	for _, t := range types {
		if t == tabletType {
			found = true
		} else {
			result = append(result, t)
		}
	}
	return result, found
}

func (wr *Wrangler) makeMastersReadOnly(shards []*topo.ShardInfo) error {
	wg := sync.WaitGroup{}
	rec := concurrency.AllErrorRecorder{}
	for _, si := range shards {
		if si.MasterAlias.IsZero() {
			rec.RecordError(fmt.Errorf("Shard %v/%v has no master?", si.Keyspace(), si.ShardName()))
			continue
		}

		wg.Add(1)
		go func(si *topo.ShardInfo) {
			defer wg.Done()

			log.Infof("Making master %v read-only", si.MasterAlias)
			actionPath, err := wr.ai.SetReadOnly(si.MasterAlias)
			if err != nil {
				rec.RecordError(err)
				return
			}
			log.Infof("Master %v is read-only", si.MasterAlias)

			rec.RecordError(wr.ai.WaitForCompletion(actionPath, wr.actionTimeout()))
		}(si)
	}
	wg.Wait()
	return rec.Error()
}

func (wr *Wrangler) getMastersPosition(shards []*topo.ShardInfo) (map[*topo.ShardInfo]*mysqlctl.ReplicationPosition, error) {
	mu := sync.Mutex{}
	result := make(map[*topo.ShardInfo]*mysqlctl.ReplicationPosition)

	wg := sync.WaitGroup{}
	rec := concurrency.AllErrorRecorder{}
	for _, si := range shards {
		wg.Add(1)
		go func(si *topo.ShardInfo) {
			defer wg.Done()
			log.Infof("Gathering master position for %v", si.MasterAlias)
			ti, err := wr.ts.GetTablet(si.MasterAlias)
			if err != nil {
				rec.RecordError(err)
				return
			}

			pos, err := wr.ai.MasterPosition(ti, wr.actionTimeout())
			if err != nil {
				rec.RecordError(err)
				return
			}

			log.Infof("Got master position for %v", si.MasterAlias)
			mu.Lock()
			result[si] = pos
			mu.Unlock()
		}(si)
	}
	wg.Wait()
	return result, rec.Error()
}

func (wr *Wrangler) waitForFilteredReplication(sourcePositions map[*topo.ShardInfo]*mysqlctl.ReplicationPosition, destinationShards []*topo.ShardInfo) error {
	wg := sync.WaitGroup{}
	rec := concurrency.AllErrorRecorder{}
	for _, si := range destinationShards {
		wg.Add(1)
		go func(si *topo.ShardInfo) {
			for _, sourceShard := range si.SourceShards {
				// we're waiting on this guy
				blpPosition := mysqlctl.BlpPosition{
					Uid: sourceShard.Uid,
				}

				// find the position it should be at
				for s, rp := range sourcePositions {
					if s.Keyspace() == sourceShard.Keyspace && s.ShardName() == sourceShard.Shard {
						blpPosition.GroupId = rp.MasterLogGroupId
					}
				}

				log.Infof("Waiting for %v to catch up", si.MasterAlias)
				if err := wr.ai.WaitBlpPosition(si.MasterAlias, blpPosition, wr.actionTimeout()); err != nil {
					rec.RecordError(err)
				} else {
					log.Infof("%v caught up", si.MasterAlias)
				}
				wg.Done()
			}
		}(si)
	}
	wg.Wait()
	return rec.Error()
}

// FIXME(alainjobart) no action to become read-write now, just use Ping,
// that forces the shard reload and will stop replication.
func (wr *Wrangler) makeMastersReadWrite(shards []*topo.ShardInfo) error {
	wg := sync.WaitGroup{}
	rec := concurrency.AllErrorRecorder{}
	for _, si := range shards {
		wg.Add(1)
		go func(si *topo.ShardInfo) {
			defer wg.Done()
			log.Infof("Pinging master %v", si.MasterAlias)

			actionPath, err := wr.ai.Ping(si.MasterAlias)
			if err != nil {
				rec.RecordError(err)
				return
			}

			if err := wr.ai.WaitForCompletion(actionPath, wr.actionTimeout()); err != nil {
				rec.RecordError(err)
			} else {
				log.Infof("%v responded", si.MasterAlias)
			}

		}(si)
	}
	wg.Wait()
	return rec.Error()
}

// migrateServedTypes operates with all concerned shards locked.
func (wr *Wrangler) migrateServedTypes(sourceShards, destinationShards []*topo.ShardInfo, servedType topo.TabletType, reverse bool) error {

	// re-read all the shards so we are up to date
	var err error
	for i, si := range sourceShards {
		if sourceShards[i], err = wr.ts.GetShard(si.Keyspace(), si.ShardName()); err != nil {
			return err
		}
	}
	for i, si := range destinationShards {
		if destinationShards[i], err = wr.ts.GetShard(si.Keyspace(), si.ShardName()); err != nil {
			return err
		}
	}

	// check and update all shard records, in memory only
	for _, si := range sourceShards {
		if reverse {
			// need to add to source
			if topo.IsTypeInList(servedType, si.ServedTypes) {
				return fmt.Errorf("Source shard %v/%v is already serving type %v", si.Keyspace(), si.ShardName(), servedType)
			}
			si.ServedTypes = append(si.ServedTypes, servedType)
		} else {
			// need to remove from source
			var found bool
			if si.ServedTypes, found = removeType(servedType, si.ServedTypes); !found {
				return fmt.Errorf("Source shard %v/%v is not serving type %v", si.Keyspace(), si.ShardName(), servedType)
			}
		}
	}
	for _, si := range destinationShards {
		if reverse {
			// need to remove from destination
			var found bool
			if si.ServedTypes, found = removeType(servedType, si.ServedTypes); !found {
				return fmt.Errorf("Destination shard %v/%v is not serving type %v", si.Keyspace(), si.ShardName(), servedType)
			}
		} else {
			// need to add to destination
			if topo.IsTypeInList(servedType, si.ServedTypes) {
				return fmt.Errorf("Destination shard %v/%v is already serving type %v", si.Keyspace(), si.ShardName(), servedType)
			}
			si.ServedTypes = append(si.ServedTypes, servedType)
		}
	}

	// For master type migration, need to:
	// - switch the source shards to read-only
	// - gather all replication points
	// - wait for filtered replication to catch up before we continue
	// - disable filtered replication after the fact
	if servedType == topo.TYPE_MASTER {
		err := wr.makeMastersReadOnly(sourceShards)
		if err != nil {
			return err
		}

		masterPositions, err := wr.getMastersPosition(sourceShards)
		if err != nil {
			return err
		}

		if err := wr.waitForFilteredReplication(masterPositions, destinationShards); err != nil {
			return err
		}

		for _, si := range destinationShards {
			si.SourceShards = nil
		}
	}

	// All is good, we can save the shards now
	for _, si := range sourceShards {
		if err := wr.ts.UpdateShard(si); err != nil {
			return err
		}
	}
	for _, si := range destinationShards {
		if err := wr.ts.UpdateShard(si); err != nil {
			return err
		}
	}

	// And tell the new shards masters they can now be read-write.
	// Invoking a remote action will also make the tablet stop filtered
	// replication.
	if servedType == topo.TYPE_MASTER {
		if err := wr.makeMastersReadWrite(destinationShards); err != nil {
			return err
		}
	}

	return nil
}
