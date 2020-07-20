package rpc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaspanet/kaspad/util/mstime"

	"github.com/kaspanet/kaspad/blockdag"
	"github.com/kaspanet/kaspad/config"
	"github.com/kaspanet/kaspad/mining"
	"github.com/kaspanet/kaspad/rpcmodel"
	"github.com/kaspanet/kaspad/txscript"
	"github.com/kaspanet/kaspad/util"
	"github.com/kaspanet/kaspad/util/daghash"
	"github.com/kaspanet/kaspad/util/random"
	"github.com/kaspanet/kaspad/wire"
	"github.com/pkg/errors"
)

const (
	// gbtNonceRange is two 64-bit big-endian hexadecimal integers which
	// represent the valid ranges of nonces returned by the getBlockTemplate
	// RPC.
	gbtNonceRange = "000000000000ffffffffffff"

	// gbtRegenerateSeconds is the number of seconds that must pass before
	// a new template is generated when the parent block hashes has not
	// changed and there have been changes to the available transactions
	// in the memory pool.
	gbtRegenerateSeconds = 60
)

var (
	// gbtMutableFields are the manipulations the server allows to be made
	// to block templates generated by the getBlockTemplate RPC. It is
	// declared here to avoid the overhead of creating the slice on every
	// invocation for constant data.
	gbtMutableFields = []string{
		"time", "transactions/add", "parentblock", "coinbase/append",
	}

	// gbtCapabilities describes additional capabilities returned with a
	// block template generated by the getBlockTemplate RPC. It is
	// declared here to avoid the overhead of creating the slice on every
	// invocation for constant data.
	gbtCapabilities = []string{"proposal"}
)

// gbtWorkState houses state that is used in between multiple RPC invocations to
// getBlockTemplate.
type gbtWorkState struct {
	sync.Mutex
	lastTxUpdate  mstime.Time
	lastGenerated mstime.Time
	tipHashes     []*daghash.Hash
	minTimestamp  mstime.Time
	template      *mining.BlockTemplate
	notifyMap     map[string]map[int64]chan struct{}
	payAddress    util.Address
}

// newGbtWorkState returns a new instance of a gbtWorkState with all internal
// fields initialized and ready to use.
func newGbtWorkState() *gbtWorkState {
	return &gbtWorkState{
		notifyMap: make(map[string]map[int64]chan struct{}),
	}
}

// builderScript is a convenience function which is used for hard-coded scripts
// built with the script builder. Any errors are converted to a panic since it
// is only, and must only, be used with hard-coded, and therefore, known good,
// scripts.
func builderScript(builder *txscript.ScriptBuilder) []byte {
	script, err := builder.Script()
	if err != nil {
		panic(err)
	}
	return script
}

// handleGetBlockTemplate implements the getBlockTemplate command.
func handleGetBlockTemplate(s *Server, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*rpcmodel.GetBlockTemplateCmd)
	request := c.Request

	// Set the default mode and override it if supplied.
	mode := "template"
	if request != nil && request.Mode != "" {
		mode = request.Mode
	}

	switch mode {
	case "template":
		return handleGetBlockTemplateRequest(s, request, closeChan)
	case "proposal":
		return handleGetBlockTemplateProposal(s, request)
	}

	return nil, &rpcmodel.RPCError{
		Code:    rpcmodel.ErrRPCInvalidParameter,
		Message: "Invalid mode",
	}
}

// handleGetBlockTemplateRequest is a helper for handleGetBlockTemplate which
// deals with generating and returning block templates to the caller. It
// handles both long poll requests as specified by BIP 0022 as well as regular
// requests.
func handleGetBlockTemplateRequest(s *Server, request *rpcmodel.TemplateRequest, closeChan <-chan struct{}) (interface{}, error) {
	// Return an error if there are no peers connected since there is no
	// way to relay a found block or receive transactions to work on.
	// However, allow this state when running in the regression test or
	// simulation test mode.
	if !(config.ActiveConfig().RegressionTest || config.ActiveConfig().Simnet) &&
		s.cfg.ConnMgr.ConnectedCount() == 0 {

		return nil, &rpcmodel.RPCError{
			Code:    rpcmodel.ErrRPCClientNotConnected,
			Message: "Kaspa is not connected",
		}
	}

	payAddr, err := util.DecodeAddress(request.PayAddress, s.cfg.DAGParams.Prefix)
	if err != nil {
		return nil, err
	}

	// When a long poll ID was provided, this is a long poll request by the
	// client to be notified when block template referenced by the ID should
	// be replaced with a new one.
	if request != nil && request.LongPollID != "" {
		return handleGetBlockTemplateLongPoll(s, request.LongPollID, payAddr, closeChan)
	}

	// Protect concurrent access when updating block templates.
	state := s.gbtWorkState
	state.Lock()
	defer state.Unlock()

	// Get and return a block template. A new block template will be
	// generated when the current best block has changed or the transactions
	// in the memory pool have been updated and it has been at least five
	// seconds since the last template was generated. Otherwise, the
	// timestamp for the existing block template is updated (and possibly
	// the difficulty on testnet per the consesus rules).
	if err := state.updateBlockTemplate(s, payAddr); err != nil {
		return nil, err
	}
	return state.blockTemplateResult(s)
}

// handleGetBlockTemplateLongPoll is a helper for handleGetBlockTemplateRequest
// which deals with handling long polling for block templates. When a caller
// sends a request with a long poll ID that was previously returned, a response
// is not sent until the caller should stop working on the previous block
// template in favor of the new one. In particular, this is the case when the
// old block template is no longer valid due to a solution already being found
// and added to the block DAG, or new transactions have shown up and some time
// has passed without finding a solution.
func handleGetBlockTemplateLongPoll(s *Server, longPollID string, payAddr util.Address, closeChan <-chan struct{}) (interface{}, error) {
	state := s.gbtWorkState

	result, longPollChan, err := blockTemplateOrLongPollChan(s, longPollID, payAddr)
	if err != nil {
		return nil, err
	}

	if result != nil {
		return result, nil
	}

	select {
	// When the client closes before it's time to send a reply, just return
	// now so the goroutine doesn't hang around.
	case <-closeChan:
		return nil, ErrClientQuit

	// Wait until signal received to send the reply.
	case <-longPollChan:
		// Fallthrough
	}

	// Get the lastest block template
	state.Lock()
	defer state.Unlock()

	if err := state.updateBlockTemplate(s, payAddr); err != nil {
		return nil, err
	}

	// Include whether or not it is valid to submit work against the old
	// block template depending on whether or not a solution has already
	// been found and added to the block DAG.
	result, err = state.blockTemplateResult(s)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// blockTemplateOrLongPollChan returns a block template if the
// template identified by the provided long poll ID is stale or
// invalid. Otherwise, it returns a channel that will notify
// when there's a more current template.
func blockTemplateOrLongPollChan(s *Server, longPollID string, payAddr util.Address) (*rpcmodel.GetBlockTemplateResult, chan struct{}, error) {
	state := s.gbtWorkState

	state.Lock()
	defer state.Unlock()
	// The state unlock is intentionally not deferred here since it needs to
	// be manually unlocked before waiting for a notification about block
	// template changes.

	if err := state.updateBlockTemplate(s, payAddr); err != nil {
		return nil, nil, err
	}

	// Just return the current block template if the long poll ID provided by
	// the caller is invalid.
	parentHashes, lastGenerated, err := decodeLongPollID(longPollID)
	if err != nil {
		result, err := state.blockTemplateResult(s)
		if err != nil {
			return nil, nil, err
		}

		return result, nil, nil
	}

	// Return the block template now if the specific block template
	// identified by the long poll ID no longer matches the current block
	// template as this means the provided template is stale.
	areHashesEqual := daghash.AreEqual(state.template.Block.Header.ParentHashes, parentHashes)
	if !areHashesEqual ||
		lastGenerated != state.lastGenerated.UnixSeconds() {

		// Include whether or not it is valid to submit work against the
		// old block template depending on whether or not a solution has
		// already been found and added to the block DAG.
		result, err := state.blockTemplateResult(s)
		if err != nil {
			return nil, nil, err
		}

		return result, nil, nil
	}

	// Register the parent hashes and last generated time for notifications
	// Get a channel that will be notified when the template associated with
	// the provided ID is stale and a new block template should be returned to
	// the caller.
	longPollChan := state.templateUpdateChan(parentHashes, lastGenerated)
	return nil, longPollChan, nil
}

// handleGetBlockTemplateProposal is a helper for handleGetBlockTemplate which
// deals with block proposals.
func handleGetBlockTemplateProposal(s *Server, request *rpcmodel.TemplateRequest) (interface{}, error) {
	hexData := request.Data
	if hexData == "" {
		return false, &rpcmodel.RPCError{
			Code: rpcmodel.ErrRPCType,
			Message: fmt.Sprintf("Data must contain the " +
				"hex-encoded serialized block that is being " +
				"proposed"),
		}
	}

	// Ensure the provided data is sane and deserialize the proposed block.
	if len(hexData)%2 != 0 {
		hexData = "0" + hexData
	}
	dataBytes, err := hex.DecodeString(hexData)
	if err != nil {
		return false, &rpcmodel.RPCError{
			Code: rpcmodel.ErrRPCDeserialization,
			Message: fmt.Sprintf("Data must be "+
				"hexadecimal string (not %q)", hexData),
		}
	}
	var msgBlock wire.MsgBlock
	if err := msgBlock.Deserialize(bytes.NewReader(dataBytes)); err != nil {
		return nil, &rpcmodel.RPCError{
			Code:    rpcmodel.ErrRPCDeserialization,
			Message: "Block decode failed: " + err.Error(),
		}
	}
	block := util.NewBlock(&msgBlock)

	// Ensure the block is building from the expected parent blocks.
	expectedParentHashes := s.cfg.DAG.TipHashes()
	parentHashes := block.MsgBlock().Header.ParentHashes
	if !daghash.AreEqual(expectedParentHashes, parentHashes) {
		return "bad-parentblk", nil
	}

	if err := s.cfg.DAG.CheckConnectBlockTemplate(block); err != nil {
		if !errors.As(err, &blockdag.RuleError{}) {
			errStr := fmt.Sprintf("Failed to process block proposal: %s", err)
			log.Error(errStr)
			return nil, &rpcmodel.RPCError{
				Code:    rpcmodel.ErrRPCVerify,
				Message: errStr,
			}
		}

		log.Infof("Rejected block proposal: %s", err)
		return dagErrToGBTErrString(err), nil
	}

	return nil, nil
}

// dagErrToGBTErrString converts an error returned from kaspa to a string
// which matches the reasons and format described in BIP0022 for rejection
// reasons.
func dagErrToGBTErrString(err error) string {
	// When the passed error is not a RuleError, just return a generic
	// rejected string with the error text.
	var ruleErr blockdag.RuleError
	if !errors.As(err, &ruleErr) {
		return "rejected: " + err.Error()
	}

	switch ruleErr.ErrorCode {
	case blockdag.ErrDuplicateBlock:
		return "duplicate"
	case blockdag.ErrBlockMassTooHigh:
		return "bad-blk-mass"
	case blockdag.ErrBlockVersionTooOld:
		return "bad-version"
	case blockdag.ErrTimeTooOld:
		return "time-too-old"
	case blockdag.ErrTimeTooNew:
		return "time-too-new"
	case blockdag.ErrDifficultyTooLow:
		return "bad-diffbits"
	case blockdag.ErrUnexpectedDifficulty:
		return "bad-diffbits"
	case blockdag.ErrHighHash:
		return "high-hash"
	case blockdag.ErrBadMerkleRoot:
		return "bad-txnmrklroot"
	case blockdag.ErrFinalityPointTimeTooOld:
		return "finality-point-time-too-old"
	case blockdag.ErrNoTransactions:
		return "bad-txns-none"
	case blockdag.ErrNoTxInputs:
		return "bad-txns-noinputs"
	case blockdag.ErrTxMassTooHigh:
		return "bad-txns-mass"
	case blockdag.ErrBadTxOutValue:
		return "bad-txns-outputvalue"
	case blockdag.ErrDuplicateTxInputs:
		return "bad-txns-dupinputs"
	case blockdag.ErrBadTxInput:
		return "bad-txns-badinput"
	case blockdag.ErrMissingTxOut:
		return "bad-txns-missinginput"
	case blockdag.ErrUnfinalizedTx:
		return "bad-txns-unfinalizedtx"
	case blockdag.ErrDuplicateTx:
		return "bad-txns-duplicate"
	case blockdag.ErrOverwriteTx:
		return "bad-txns-overwrite"
	case blockdag.ErrImmatureSpend:
		return "bad-txns-maturity"
	case blockdag.ErrSpendTooHigh:
		return "bad-txns-highspend"
	case blockdag.ErrBadFees:
		return "bad-txns-fees"
	case blockdag.ErrTooManySigOps:
		return "high-sigops"
	case blockdag.ErrFirstTxNotCoinbase:
		return "bad-txns-nocoinbase"
	case blockdag.ErrMultipleCoinbases:
		return "bad-txns-multicoinbase"
	case blockdag.ErrBadCoinbasePayloadLen:
		return "bad-cb-length"
	case blockdag.ErrScriptMalformed:
		return "bad-script-malformed"
	case blockdag.ErrScriptValidation:
		return "bad-script-validate"
	case blockdag.ErrParentBlockUnknown:
		return "parent-blk-not-found"
	case blockdag.ErrInvalidAncestorBlock:
		return "bad-parentblk"
	case blockdag.ErrParentBlockNotCurrentTips:
		return "inconclusive-not-best-parentblk"
	}

	return "rejected: " + err.Error()
}

// notifyLongPollers notifies any channels that have been registered to be
// notified when block templates are stale.
//
// This function MUST be called with the state locked.
func (state *gbtWorkState) notifyLongPollers(tipHashes []*daghash.Hash, lastGenerated mstime.Time) {
	// Notify anything that is waiting for a block template update from
	// hashes which are not the current tip hashes.
	tipHashesStr := daghash.JoinHashesStrings(tipHashes, "")
	for hashesStr, channels := range state.notifyMap {
		if hashesStr != tipHashesStr {
			for _, c := range channels {
				close(c)
			}
			delete(state.notifyMap, hashesStr)
		}
	}

	// Return now if the provided last generated timestamp has not been
	// initialized.
	if lastGenerated.IsZero() {
		return
	}

	// Return now if there is nothing registered for updates to the current
	// best block hash.
	channels, ok := state.notifyMap[tipHashesStr]
	if !ok {
		return
	}

	// Notify anything that is waiting for a block template update from a
	// block template generated before the most recently generated block
	// template.
	lastGeneratedUnix := lastGenerated.UnixSeconds()
	for lastGen, c := range channels {
		if lastGen < lastGeneratedUnix {
			close(c)
			delete(channels, lastGen)
		}
	}

	// Remove the entry altogether if there are no more registered
	// channels.
	if len(channels) == 0 {
		delete(state.notifyMap, tipHashesStr)
	}
}

// NotifyBlockAdded uses the newly-added block to notify any long poll
// clients with a new block template when their existing block template is
// stale due to the newly added block.
func (state *gbtWorkState) NotifyBlockAdded(tipHashes []*daghash.Hash) {
	spawn("gbtWorkState.NotifyBlockAdded", func() {
		state.Lock()
		defer state.Unlock()

		state.notifyLongPollers(tipHashes, state.lastTxUpdate)
	})
}

// NotifyMempoolTx uses the new last updated time for the transaction memory
// pool to notify any long poll clients with a new block template when their
// existing block template is stale due to enough time passing and the contents
// of the memory pool changing.
func (state *gbtWorkState) NotifyMempoolTx(lastUpdated mstime.Time) {
	spawn("NotifyMempoolTx", func() {
		state.Lock()
		defer state.Unlock()

		// No need to notify anything if no block templates have been generated
		// yet.
		if state.tipHashes == nil || state.lastGenerated.IsZero() {
			return
		}

		if mstime.Now().After(state.lastGenerated.Add(time.Second *
			gbtRegenerateSeconds)) {

			state.notifyLongPollers(state.tipHashes, lastUpdated)
		}
	})
}

// templateUpdateChan returns a channel that will be closed once the block
// template associated with the passed parent hashes and last generated time
// is stale. The function will return existing channels for duplicate
// parameters which allows multiple clients to wait for the same block template
// without requiring a different channel for each client.
//
// This function MUST be called with the state locked.
func (state *gbtWorkState) templateUpdateChan(tipHashes []*daghash.Hash, lastGenerated int64) chan struct{} {
	tipHashesStr := daghash.JoinHashesStrings(tipHashes, "")
	// Either get the current list of channels waiting for updates about
	// changes to block template for the parent hashes or create a new one.
	channels, ok := state.notifyMap[tipHashesStr]
	if !ok {
		m := make(map[int64]chan struct{})
		state.notifyMap[tipHashesStr] = m
		channels = m
	}

	// Get the current channel associated with the time the block template
	// was last generated or create a new one.
	c, ok := channels[lastGenerated]
	if !ok {
		c = make(chan struct{})
		channels[lastGenerated] = c
	}

	return c
}

// updateBlockTemplate creates or updates a block template for the work state.
// A new block template will be generated when the current best block has
// changed or the transactions in the memory pool have been updated and it has
// been long enough since the last template was generated. Otherwise, the
// timestamp for the existing block template is updated (and possibly the
// difficulty on testnet per the consesus rules). Finally, if the
// useCoinbaseValue flag is false and the existing block template does not
// already contain a valid payment address, the block template will be updated
// with a randomly selected payment address from the list of configured
// addresses.
//
// This function MUST be called with the state locked.
func (state *gbtWorkState) updateBlockTemplate(s *Server, payAddr util.Address) error {
	generator := s.cfg.Generator
	lastTxUpdate := generator.TxSource().LastUpdated()
	if lastTxUpdate.IsZero() {
		lastTxUpdate = mstime.Now()
	}

	// Generate a new block template when the current best block has
	// changed or the transactions in the memory pool have been updated and
	// it has been at least gbtRegenerateSecond since the last template was
	// generated.
	var msgBlock *wire.MsgBlock
	var targetDifficulty string
	tipHashes := s.cfg.DAG.TipHashes()
	template := state.template
	if template == nil || state.tipHashes == nil ||
		!daghash.AreEqual(state.tipHashes, tipHashes) ||
		state.payAddress.String() != payAddr.String() ||
		(state.lastTxUpdate != lastTxUpdate &&
			mstime.Now().After(state.lastGenerated.Add(time.Second*
				gbtRegenerateSeconds))) {

		// Reset the previous best hash the block template was generated
		// against so any errors below cause the next invocation to try
		// again.
		state.tipHashes = nil

		// Create a new block template that has a coinbase which anyone
		// can redeem. This is only acceptable because the returned
		// block template doesn't include the coinbase, so the caller
		// will ultimately create their own coinbase which pays to the
		// appropriate address(es).

		extraNonce, err := random.Uint64()
		if err != nil {
			return internalRPCError(fmt.Sprintf("Failed to randomize "+
				"extra nonce: %s", err.Error()), "")
		}

		blkTemplate, err := generator.NewBlockTemplate(payAddr, extraNonce)
		if err != nil {
			return internalRPCError(fmt.Sprintf("Failed to create new block "+
				"template: %s", err.Error()), "")
		}
		template = blkTemplate
		msgBlock = template.Block
		targetDifficulty = fmt.Sprintf("%064x",
			util.CompactToBig(msgBlock.Header.Bits))

		// Get the minimum allowed timestamp for the block based on the
		// median timestamp of the last several blocks per the DAG
		// consensus rules.
		minTimestamp := s.cfg.DAG.NextBlockMinimumTime()

		// Update work state to ensure another block template isn't
		// generated until needed.
		state.template = template
		state.lastGenerated = mstime.Now()
		state.lastTxUpdate = lastTxUpdate
		state.tipHashes = tipHashes
		state.minTimestamp = minTimestamp
		state.payAddress = payAddr

		log.Debugf("Generated block template (timestamp %s, "+
			"target %s, merkle root %s)",
			msgBlock.Header.Timestamp, targetDifficulty,
			msgBlock.Header.HashMerkleRoot)

		// Notify any clients that are long polling about the new
		// template.
		state.notifyLongPollers(tipHashes, lastTxUpdate)
	} else {
		// At this point, there is a saved block template and another
		// request for a template was made, but either the available
		// transactions haven't change or it hasn't been long enough to
		// trigger a new block template to be generated. So, update the
		// existing block template.

		// Set locals for convenience.
		msgBlock = template.Block
		targetDifficulty = fmt.Sprintf("%064x",
			util.CompactToBig(msgBlock.Header.Bits))

		// Update the time of the block template to the current time
		// while accounting for the median time of the past several
		// blocks per the DAG consensus rules.
		generator.UpdateBlockTime(msgBlock)
		msgBlock.Header.Nonce = 0

		log.Debugf("Updated block template (timestamp %s, "+
			"target %s)", msgBlock.Header.Timestamp,
			targetDifficulty)
	}

	return nil
}

// blockTemplateResult returns the current block template associated with the
// state as a rpcmodel.GetBlockTemplateResult that is ready to be encoded to JSON
// and returned to the caller.
//
// This function MUST be called with the state locked.
func (state *gbtWorkState) blockTemplateResult(s *Server) (*rpcmodel.GetBlockTemplateResult, error) {
	dag := s.cfg.DAG
	// Ensure the timestamps are still in valid range for the template.
	// This should really only ever happen if the local clock is changed
	// after the template is generated, but it's important to avoid serving
	// block templates that will be delayed on other nodes.
	template := state.template
	msgBlock := template.Block
	header := &msgBlock.Header
	adjustedTime := dag.Now()
	maxTime := adjustedTime.Add(time.Millisecond * time.Duration(dag.TimestampDeviationTolerance))
	if header.Timestamp.After(maxTime) {
		return nil, &rpcmodel.RPCError{
			Code: rpcmodel.ErrRPCOutOfRange,
			Message: fmt.Sprintf("The template time is after the "+
				"maximum allowed time for a block - template "+
				"time %s, maximum time %s", adjustedTime,
				maxTime),
		}
	}

	// Convert each transaction in the block template to a template result
	// transaction. The result does not include the coinbase, so notice
	// the adjustments to the various lengths and indices.
	numTx := len(msgBlock.Transactions)
	transactions := make([]rpcmodel.GetBlockTemplateResultTx, 0, numTx-1)
	txIndex := make(map[daghash.TxID]int64, numTx)
	for i, tx := range msgBlock.Transactions {
		txID := tx.TxID()
		txIndex[*txID] = int64(i)

		// Create an array of 1-based indices to transactions that come
		// before this one in the transactions list which this one
		// depends on. This is necessary since the created block must
		// ensure proper ordering of the dependencies. A map is used
		// before creating the final array to prevent duplicate entries
		// when multiple inputs reference the same transaction.
		dependsMap := make(map[int64]struct{})
		for _, txIn := range tx.TxIn {
			if idx, ok := txIndex[txIn.PreviousOutpoint.TxID]; ok {
				dependsMap[idx] = struct{}{}
			}
		}
		depends := make([]int64, 0, len(dependsMap))
		for idx := range dependsMap {
			depends = append(depends, idx)
		}

		// Serialize the transaction for later conversion to hex.
		txBuf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
		if err := tx.Serialize(txBuf); err != nil {
			context := "Failed to serialize transaction"
			return nil, internalRPCError(err.Error(), context)
		}

		resultTx := rpcmodel.GetBlockTemplateResultTx{
			Data:    hex.EncodeToString(txBuf.Bytes()),
			ID:      txID.String(),
			Depends: depends,
			Mass:    template.TxMasses[i],
			Fee:     template.Fees[i],
		}
		transactions = append(transactions, resultTx)
	}

	// Generate the block template reply. Note that following mutations are
	// implied by the included or omission of fields:
	//  Including MinTime -> time/decrement
	//  Omitting CoinbaseTxn -> coinbase, generation
	targetDifficulty := fmt.Sprintf("%064x", util.CompactToBig(header.Bits))
	longPollID := encodeLongPollID(state.tipHashes, state.payAddress, state.lastGenerated)

	// Check whether this node is synced with the rest of of the
	// network. There's almost never a good reason to mine on top
	// of an unsynced DAG, and miners are generally expected not to
	// mine when isSynced is false.
	// This is not a straight-up error because the choice of whether
	// to mine or not is the responsibility of the miner rather
	// than the node's.
	isSynced := s.cfg.SyncMgr.IsSynced()

	reply := rpcmodel.GetBlockTemplateResult{
		Bits:                 strconv.FormatInt(int64(header.Bits), 16),
		CurTime:              header.Timestamp.UnixMilliseconds(),
		Height:               template.Height,
		ParentHashes:         daghash.Strings(header.ParentHashes),
		MassLimit:            wire.MaxMassPerBlock,
		Transactions:         transactions,
		HashMerkleRoot:       header.HashMerkleRoot.String(),
		AcceptedIDMerkleRoot: header.AcceptedIDMerkleRoot.String(),
		UTXOCommitment:       header.UTXOCommitment.String(),
		Version:              header.Version,
		LongPollID:           longPollID,
		Target:               targetDifficulty,
		MinTime:              state.minTimestamp.UnixMilliseconds(),
		MaxTime:              maxTime.UnixMilliseconds(),
		Mutable:              gbtMutableFields,
		NonceRange:           gbtNonceRange,
		Capabilities:         gbtCapabilities,
		IsSynced:             isSynced,
	}

	return &reply, nil
}

// encodeLongPollID encodes the passed details into an ID that can be used to
// uniquely identify a block template.
func encodeLongPollID(parentHashes []*daghash.Hash, miningAddress util.Address, lastGenerated mstime.Time) string {
	return fmt.Sprintf("%s-%s-%d", daghash.JoinHashesStrings(parentHashes, ""), miningAddress, lastGenerated.UnixSeconds())
}

// decodeLongPollID decodes an ID that is used to uniquely identify a block
// template. This is mainly used as a mechanism to track when to update clients
// that are using long polling for block templates. The ID consists of the
// parent blocks hashes for the associated template and the time the associated
// template was generated.
func decodeLongPollID(longPollID string) ([]*daghash.Hash, int64, error) {
	fields := strings.Split(longPollID, "-")
	if len(fields) != 2 {
		return nil, 0, errors.New("decodeLongPollID: invalid number of fields")
	}

	parentHashesStr := fields[0]
	if len(parentHashesStr)%daghash.HashSize != 0 {
		return nil, 0, errors.New("decodeLongPollID: invalid parent hashes format")
	}
	numberOfHashes := len(parentHashesStr) / daghash.HashSize

	parentHashes := make([]*daghash.Hash, 0, numberOfHashes)

	for i := 0; i < len(parentHashesStr); i += daghash.HashSize {
		hash, err := daghash.NewHashFromStr(parentHashesStr[i : i+daghash.HashSize])
		if err != nil {
			return nil, 0, errors.Errorf("decodeLongPollID: NewHashFromStr: %s", err)
		}
		parentHashes = append(parentHashes, hash)
	}

	lastGenerated, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return nil, 0, errors.Errorf("decodeLongPollID: Cannot parse timestamp %s: %s", fields[1], err)
	}

	return parentHashes, lastGenerated, nil
}
