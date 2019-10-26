package mining

// Block generation is part of the logic of the DefaultWorker.
// 'generate' is that function that actually creates a new block from a base
// TipSet using the DefaultWorker's many utilities.

import (
	"context"
	"time"

	"github.com/filecoin-project/go-bls-sigs"
	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/internal/pkg/address"
	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm"
)

// Generate returns a new block created from the messages in the pool.
func (w *DefaultWorker) Generate(ctx context.Context,
	baseTipSet block.TipSet,
	tickets []block.Ticket,
	electionProof block.VRFPi,
	nullBlockCount uint64) (*block.Block, error) {

	generateTimer := time.Now()
	defer func() {
		log.Infof("[TIMER] DefaultWorker.Generate baseTipset: %s - elapsed time: %s", baseTipSet.String(), time.Since(generateTimer).Round(time.Millisecond))
	}()

	stateTree, err := w.getStateTree(ctx, baseTipSet)
	if err != nil {
		return nil, errors.Wrap(err, "get state tree")
	}

	powerTable, err := w.getPowerTable(ctx, baseTipSet.Key())
	if err != nil {
		return nil, errors.Wrap(err, "get power table")
	}

	if !powerTable.HasPower(ctx, w.minerAddr) {
		return nil, errors.Errorf("bad miner address, miner must store files before mining: %s", w.minerAddr)
	}

	weight, err := w.getWeight(ctx, baseTipSet)
	if err != nil {
		return nil, errors.Wrap(err, "get weight")
	}

	baseHeight, err := baseTipSet.Height()
	if err != nil {
		return nil, errors.Wrap(err, "get base tip set height")
	}

	blockHeight := baseHeight + nullBlockCount + 1

	ancestors, err := w.getAncestors(ctx, baseTipSet, types.NewBlockHeight(blockHeight))
	if err != nil {
		return nil, errors.Wrap(err, "get base tip set ancestors")
	}

	pending := w.messageSource.Pending()
	mq := NewMessageQueue(pending)
	secpMessages, blsMessages := divideMessages(mq.Drain())

	// bls messages are processed first
	messages := append(blsMessages, secpMessages...)

	vms := vm.NewStorageMap(w.blockstore)
	res, err := w.processor.ApplyMessagesAndPayRewards(ctx, stateTree, vms, messages, w.minerOwnerAddr, types.NewBlockHeight(blockHeight), ancestors)
	if err != nil {
		return nil, errors.Wrap(err, "generate apply messages")
	}

	newStateTreeCid, err := stateTree.Flush(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "generate flush state tree")
	}

	if err = vms.Flush(); err != nil {
		return nil, errors.Wrap(err, "generate flush vm storage map")
	}

	// By default no receipts/messages is serialized as the zero length
	// slice, not the nil slice.
	receipts := []*types.MessageReceipt{}
	for _, r := range res.Results {
		receipts = append(receipts, r.Receipt)
	}

	// split mined messages into secp and bls
	minedSecpMessages, minedBLSMessages := divideMessages(res.SuccessfulMessages)

	// create an aggregage signature for messages
	unwrappedBLSMessages, blsAggregateSig, err := aggregateBLS(minedBLSMessages)
	if err != nil {
		return nil, errors.Wrap(err, "could not aggregate bls messages")
	}

	// Persist messages to ipld storage
	txMeta, err := w.messageStore.StoreMessages(ctx, minedSecpMessages, unwrappedBLSMessages)
	if err != nil {
		return nil, errors.Wrap(err, "error persisting messages")
	}
	rcptsCid, err := w.messageStore.StoreReceipts(ctx, receipts)
	if err != nil {
		return nil, errors.Wrap(err, "error persisting receipts")
	}

	next := &block.Block{
		Miner:           w.minerAddr,
		Height:          types.Uint64(blockHeight),
		Messages:        txMeta,
		MessageReceipts: rcptsCid,
		Parents:         baseTipSet.Key(),
		ParentWeight:    types.Uint64(weight),
		ElectionProof:   electionProof,
		StateRoot:       newStateTreeCid,
		Tickets:         tickets,
		Timestamp:       types.Uint64(w.clock.Now().Unix()),
		BLSAggregateSig: blsAggregateSig,
	}
	workerAddr, err := w.api.MinerGetWorkerAddress(ctx, w.minerAddr, baseTipSet.Key())
	if err != nil {
		return nil, errors.Wrap(err, "failed to read workerAddr during block generation")
	}
	next.BlockSig, err = w.workerSigner.SignBytes(next.SignatureData(), workerAddr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to sign block")
	}

	for i, msg := range res.PermanentFailures {
		// We will not be able to apply this message in the future because the error was permanent.
		// Therefore, we will remove it from the MessagePool now.
		// There might be better places to do this, such as wherever successful messages are removed
		// from the pool, or by posting the failure to an event bus to be handled async.
		log.Infof("permanent ApplyMessage failure, [%s] (%s)", msg, res.PermanentErrors[i])
		mc, err := msg.Cid()
		if err == nil {
			w.messageSource.Remove(mc)
		} else {
			log.Warnf("failed to get CID from message", err)
		}
	}

	for i, msg := range res.TemporaryFailures {
		// We might be able to apply this message in the future because the error was temporary.
		// Therefore, we will leave it in the MessagePool for now.

		log.Infof("temporary ApplyMessage failure, [%s] (%s)", msg, res.TemporaryErrors[i])
	}

	return next, nil
}

func aggregateBLS(blsMessages []*types.SignedMessage) ([]*types.UnsignedMessage, types.Signature, error) {
	sigs := []bls.Signature{}
	unwrappedMsgs := []*types.UnsignedMessage{}
	for _, msg := range blsMessages {
		// unwrap messages
		unwrappedMsgs = append(unwrappedMsgs, &msg.Message)
		sig := msg.Signature

		// store message signature as bls signature
		blsSig := bls.Signature{}
		copy(blsSig[:], sig)
		sigs = append(sigs, blsSig)
	}
	blsAggregateSig := bls.Aggregate(sigs)
	if blsAggregateSig == nil {
		return []*types.UnsignedMessage{}, types.Signature{}, errors.New("could not aggregate signatures")
	}
	return unwrappedMsgs, blsAggregateSig[:], nil
}

func divideMessages(messages []*types.SignedMessage) ([]*types.SignedMessage, []*types.SignedMessage) {
	secpMessages := []*types.SignedMessage{}
	blsMessages := []*types.SignedMessage{}

	for _, m := range messages {
		if m.Message.From.Protocol() == address.BLS {
			blsMessages = append(blsMessages, m)
		} else {
			secpMessages = append(secpMessages, m)
		}
	}
	return secpMessages, blsMessages
}