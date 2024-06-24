package blsagg

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/Layr-Labs/eigensdk-go/crypto/bls"
	"github.com/Layr-Labs/eigensdk-go/logging"
	"github.com/Layr-Labs/eigensdk-go/services/avsregistry"
	"github.com/Layr-Labs/eigensdk-go/types"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
)

var (
	TaskAlreadyInitializedErrorFn = func(taskIndex types.TaskIndex) error {
		return fmt.Errorf("task %d already initialized", taskIndex)
	}
	TaskExpiredError      = fmt.Errorf("task expired")
	TaskNotRespondedError = fmt.Errorf("task expired with zero responses")
	TaskNotFoundErrorFn   = func(taskIndex types.TaskIndex) error {
		return fmt.Errorf("task %d not initialized or already completed", taskIndex)
	}
	OperatorNotPartOfTaskQuorumErrorFn = func(operatorId types.OperatorId, taskIndex types.TaskIndex) error {
		return fmt.Errorf("operator %x not part of task %d's quorum", operatorId, taskIndex)
	}
	SignatureVerificationError = func(err error) error {
		return fmt.Errorf("Failed to verify signature: %w", err)
	}
	IncorrectSignatureError = errors.New("Signature verification failed. Incorrect Signature.")
)

// BlsAggregationServiceResponse is the response from the bls aggregation service
type BlsAggregationServiceResponse struct {
	Err                error                    // if Err is not nil, the other fields are not valid
	TaskIndex          types.TaskIndex          // unique identifier of the task
	TaskResponseDigest types.TaskResponseDigest // digest of the task response that was signed
	// The below 8 fields are the data needed to build the IBLSSignatureChecker.NonSignerStakesAndSignature struct
	// users of this service will need to build the struct themselves by converting the bls points
	// into the BN254.G1/G2Point structs that the IBLSSignatureChecker expects
	// given that those are different for each AVS service manager that individually inherits BLSSignatureChecker
	NonSignersPubkeysG1          []*bls.G1Point
	QuorumApksG1                 []*bls.G1Point
	SignersApkG2                 *bls.G2Point
	SignersAggSigG1              *bls.Signature
	NonSignerQuorumBitmapIndices []uint32
	QuorumApkIndices             []uint32
	TotalStakeIndices            []uint32
	NonSignerStakeIndices        [][]uint32
	OldSignersApkG2             *bls.G2Point
	OldSignersAggSigG1          *bls.Signature
	NonSignerPubkeysIndicesforOperatorIdsRemovedForOldState   []uint32
	NonSignerPubkeysAddedForOldState   []*bls.G1Point
}

// aggregatedOperators is meant to be used as a value in a map
// map[taskResponseDigest]aggregatedOperators
type aggregatedOperators struct {
	// aggregate g2 pubkey of all operatos who signed on this taskResponseDigest
	signersApkG2 *bls.G2Point
	// aggregate signature of all operators who signed on this taskResponseDigest
	signersAggSigG1 *bls.Signature
	// aggregate stake of all operators who signed on this header for each quorum
	signersTotalStakePerQuorum map[types.QuorumNum]*big.Int
	// set of OperatorId of operators who signed on this header
	signersOperatorIdsSet map[types.OperatorId]bool
	oldSignersApkG2 *bls.G2Point
	oldSignersAggSigG1 *bls.Signature
	oldSignersOperatorIdsSet map[types.OperatorId]bool
}

// BlsAggregationService is the interface provided to avs aggregator code for doing bls aggregation
// Currently its only implementation is the BlsAggregatorService, so see the comment there for more details
type BlsAggregationService interface {
	// InitializeNewTask should be called whenever a new task is created. ProcessNewSignature will return an error
	// if the task it is trying to process has not been initialized yet.
	// quorumNumbers and quorumThresholdPercentages set the requirements for this task to be considered complete, which happens
	// when a particular TaskResponseDigest (received via the a.taskChans[taskIndex]) has been signed by signers whose stake
	// in each of the listed quorums adds up to at least quorumThresholdPercentages[i] of the total stake in that quorum
	InitializeNewTask(
		taskIndex types.TaskIndex,
		taskCreatedBlock uint32,
		quorumNumbers types.QuorumNums,
		lastCompletedTaskCreatedBlock uint32,
		lastCompletedTaskQuorumNumbers types.QuorumNums,
		quorumThresholdPercentages types.QuorumThresholdPercentages,
		timeToExpiry time.Duration,
	) error

	// ProcessNewSignature processes a new signature over a taskResponseDigest for a particular taskIndex by a particular operator
	// It verifies that the signature is correct and returns an error if it is not, and then aggregates the signature and stake of
	// the operator with all other signatures for the same taskIndex and taskResponseDigest pair.
	// Note: This function currently only verifies signatures over the taskResponseDigest directly, so avs code needs to verify that the digest
	// passed to ProcessNewSignature is indeed the digest of a valid taskResponse (that is, BlsAggregationService does not verify semantic integrity of the taskResponses)
	ProcessNewSignature(
		ctx context.Context,
		taskIndex types.TaskIndex,
		taskResponseDigest types.TaskResponseDigest,
		blsSignature *bls.Signature,
		operatorId types.OperatorId,
	) error

	// GetResponseChannel returns the single channel that meant to be used as the response channel
	// Any task that is completed (see the completion criterion in the comment above InitializeNewTask)
	// will be sent on this channel along with all the necessary information to call BLSSignatureChecker onchain
	GetResponseChannel() <-chan BlsAggregationServiceResponse
}

// BlsAggregatorService is a service that performs BLS signature aggregation for an AVS' tasks
// Assumptions:
//  1. BlsAggregatorService only verifies digest signatures, so avs code needs to verify that the digest
//     passed to ProcessNewSignature is indeed the digest of a valid taskResponse
//     (see the comment above checkSignature for more details)
//  2. BlsAggregatorService is VERY generic and makes very few assumptions about the tasks structure or
//     the time at which operators will send their signatures. It is mostly suitable for offchain computation
//     oracle (a la truebit) type of AVS, where tasks are sent onchain by users sporadically, and where
//     new tasks can start even before the previous ones have finished aggregation.
//     AVSs like eigenDA that have a much more controlled task submission schedule and where new tasks are
//     only submitted after the previous one's response has been aggregated and responded onchain, could have
//     a much simpler AggregationService without all the complicated parallel goroutines.
type BlsAggregatorService struct {
	// aggregatedResponsesC is the channel which all goroutines share to send their responses back to the
	// main thread after they are done aggregating (either they reached the threshold, or timeout expired)
	aggregatedResponsesC chan BlsAggregationServiceResponse
	// signedTaskRespsCs are the channels to send the signed task responses to the goroutines processing them
	// each new task is assigned a new goroutine and a new channel
	signedTaskRespsCs map[types.TaskIndex]chan types.SignedTaskResponseDigest
	// we add chans to taskChans from the main thread (InitializeNewTask) when we create new tasks,
	// we read them in ProcessNewSignature from the main thread when we receive new signed tasks,
	// and remove them from its respective goroutine when the task is completed or reached timeout
	// we thus need a mutex to protect taskChans
	taskChansMutex     sync.RWMutex
	avsRegistryService avsregistry.AvsRegistryService
	logger             logging.Logger
	debounceRpc        int
}

var _ BlsAggregationService = (*BlsAggregatorService)(nil)

func NewBlsAggregatorService(avsRegistryService avsregistry.AvsRegistryService, debounceRpc int, logger logging.Logger) *BlsAggregatorService {
	return &BlsAggregatorService{
		aggregatedResponsesC: make(chan BlsAggregationServiceResponse),
		signedTaskRespsCs:    make(map[types.TaskIndex]chan types.SignedTaskResponseDigest),
		taskChansMutex:       sync.RWMutex{},
		avsRegistryService:   avsRegistryService,
		debounceRpc: debounceRpc,
		logger: logger,
	}
}

func (a *BlsAggregatorService) GetResponseChannel() <-chan BlsAggregationServiceResponse {
	return a.aggregatedResponsesC
}

// InitializeNewTask creates a new task goroutine meant to process new signed task responses for that task
// (that are sent via ProcessNewSignature) and adds a channel to a.taskChans to send the signed task responses to it
// quorumNumbers and quorumThresholdPercentages set the requirements for this task to be considered complete, which happens
// when a particular TaskResponseDigest (received via the a.taskChans[taskIndex]) has been signed by signers whose stake
// in each of the listed quorums adds up to at least quorumThresholdPercentages[i] of the total stake in that quorum
func (a *BlsAggregatorService) InitializeNewTask(
	taskIndex types.TaskIndex,
	taskCreatedBlock uint32,
	quorumNumbers types.QuorumNums,
	lastCompletedTaskCreatedBlock uint32,
	lastCompletedTaskQuorumNumbers types.QuorumNums,
	quorumThresholdPercentages types.QuorumThresholdPercentages,
	timeToExpiry time.Duration,
) error {
	a.logger.Debug("AggregatorService initializing new task", "taskIndex", taskIndex, "taskCreatedBlock", taskCreatedBlock, "quorumNumbers", quorumNumbers, "quorumThresholdPercentages", quorumThresholdPercentages, "timeToExpiry", timeToExpiry)
	if _, taskExists := a.signedTaskRespsCs[taskIndex]; taskExists {
		return TaskAlreadyInitializedErrorFn(taskIndex)
	}
	signedTaskRespsC := make(chan types.SignedTaskResponseDigest)
	a.taskChansMutex.Lock()
	a.signedTaskRespsCs[taskIndex] = signedTaskRespsC
	a.taskChansMutex.Unlock()
	go a.singleTaskAggregatorGoroutineFunc(taskIndex, taskCreatedBlock, quorumNumbers, lastCompletedTaskCreatedBlock, lastCompletedTaskQuorumNumbers, quorumThresholdPercentages, timeToExpiry, signedTaskRespsC)
	return nil
}

func (a *BlsAggregatorService) ProcessNewSignature(
	ctx context.Context,
	taskIndex types.TaskIndex,
	taskResponseDigest types.TaskResponseDigest,
	blsSignature *bls.Signature,
	operatorId types.OperatorId,
) error {
	a.taskChansMutex.Lock()
	taskC, taskInitialized := a.signedTaskRespsCs[taskIndex]
	a.taskChansMutex.Unlock()
	if !taskInitialized {
		return TaskNotFoundErrorFn(taskIndex)
	}
	signatureVerificationErrorC := make(chan error)
	// send the task to the goroutine processing this task
	// and return the error (if any) returned by the signature verification routine
	select {
	// we need to send this as part of select because if the goroutine is processing another SignedTaskResponseDigest
	// and cannot receive this one, we want the context to be able to cancel the request
	case taskC <- types.SignedTaskResponseDigest{
		TaskResponseDigest:          taskResponseDigest,
		BlsSignature:                blsSignature,
		OperatorId:                  operatorId,
		SignatureVerificationErrorC: signatureVerificationErrorC,
	}:
		// note that we need to wait synchronously here for this response because we want to
		// send back an informative error message to the operator who sent his signature to the aggregator
		return <-signatureVerificationErrorC
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *BlsAggregatorService) singleTaskAggregatorGoroutineFunc(
	taskIndex types.TaskIndex,
	taskCreatedBlock uint32,
	quorumNumbers types.QuorumNums,
	lastCompletedTaskCreatedBlock uint32,
	lastCompletedTaskQuorumNumbers types.QuorumNums,
	quorumThresholdPercentages []types.QuorumThresholdPercentage,
	timeToExpiry time.Duration,
	signedTaskRespsC <-chan types.SignedTaskResponseDigest,
) {
	defer a.closeTaskGoroutine(taskIndex)

	timeDebounce := time.Second * time.Duration(a.debounceRpc)

	quorumThresholdPercentagesMap := make(map[types.QuorumNum]types.QuorumThresholdPercentage)
	for i, quorumNumber := range quorumNumbers {
		quorumThresholdPercentagesMap[quorumNumber] = quorumThresholdPercentages[i]
	}
	operatorsAvsStateDict, err := a.avsRegistryService.GetOperatorsAvsStateAtBlock(context.Background(), quorumNumbers, taskCreatedBlock)
	if err != nil {
		// TODO: how should we handle such an error?
		a.logger.Fatal("AggregatorService failed to get operators state from avs registry", "err", err, "blockNumber", taskCreatedBlock)
	}
	oldOperatorsAvsStateDict, err := a.avsRegistryService.GetOperatorsAvsStateAtBlock(context.Background(), lastCompletedTaskQuorumNumbers, lastCompletedTaskCreatedBlock)
	if err != nil {
		// TODO: how should we handle such an error?
		a.logger.Fatal("Aggregator failed to get operators state from avs registry", "err", err)
	}

	// var operators_added map[types.OperatorId]bool
	// var operators_removed map[types.OperatorId]bool

	// for k, _ := range operatorsAvsStateDict{
	// 	_, ok := oldOperatorsAvsStateDict[k] 
	// 	if !ok{
	// 		operators_added[k] = true
	// 	}
	// }
	// for k, _ := range oldOperatorsAvsStateDict{
	// 	_, ok := operatorsAvsStateDict[k] 
	// 	if !ok{
	// 		operators_removed[k] = true
	// 	}
	// }

	quorumsAvsStakeDict, err := a.avsRegistryService.GetQuorumsAvsStateAtBlock(context.Background(), quorumNumbers, taskCreatedBlock)
	if err != nil {
		a.logger.Fatal("Aggregator failed to get quorums state from avs registry", "err", err)
	}
	totalStakePerQuorum := make(map[types.QuorumNum]*big.Int)
	for quorumNum, quorumAvsState := range quorumsAvsStakeDict {
		totalStakePerQuorum[quorumNum] = quorumAvsState.TotalStake
	}
	quorumApksG1 := []*bls.G1Point{}
	for _, quorumNumber := range quorumNumbers {
		quorumApksG1 = append(quorumApksG1, quorumsAvsStakeDict[quorumNumber].AggPubkeyG1)
	}

	aggregatedOperatorsDict := map[types.TaskResponseDigest]aggregatedOperators{}

	// TODO(samlaf): instead of taking a TTE, we should take a block as input
	// and monitor the chain and only close the task goroutine when that block is reached
	taskExpiredTimer := time.NewTimer(timeToExpiry)
	// set initialy after expired timer, reset on each new response received
	taskResponseDebounceTimer := time.NewTimer(timeToExpiry + timeDebounce)

	for {
		select {
		case signedTaskResponseDigest := <-signedTaskRespsC:
			a.logger.Debug("Task goroutine received new signed task response digest", "taskIndex", taskIndex, "signedTaskResponseDigest", signedTaskResponseDigest)

			// we expect all operators to respond within the same window, if there are no more responses within timeDebounce
			// proceed to finishing the task. We want to avoid omiting valid responses just because the threshold was already met.
			taskResponseDebounceTimer.Stop()
			taskResponseDebounceTimer.Reset(timeDebounce)

			err := a.verifySignature(taskIndex, signedTaskResponseDigest, operatorsAvsStateDict, oldOperatorsAvsStateDict)
			signedTaskResponseDigest.SignatureVerificationErrorC <- err
			if err != nil {
				continue
			}

			// after verifying signature we aggregate its sig and pubkey, and update the signed stake amount
			digestAggregatedOperators, ok := aggregatedOperatorsDict[signedTaskResponseDigest.TaskResponseDigest]
			if !ok {
				digestAggregatedOperators = aggregatedOperators{
					signersApkG2:               bls.NewZeroG2Point(),
					signersAggSigG1:            bls.NewZeroSignature(),
					signersOperatorIdsSet:      map[types.OperatorId]bool{},
					signersTotalStakePerQuorum: map[types.QuorumNum]*big.Int{},
					oldSignersApkG2:            bls.NewZeroG2Point(),
					oldSignersAggSigG1:         bls.NewZeroSignature(),
					oldSignersOperatorIdsSet:   map[types.OperatorId]bool{},
				}
			}
			// Has to be in one of the two below
			if _, ok := oldOperatorsAvsStateDict[signedTaskResponseDigest.OperatorId]; ok {
				digestAggregatedOperators.oldSignersAggSigG1.Add(signedTaskResponseDigest.BlsSignature)
				digestAggregatedOperators.oldSignersApkG2.Add(oldOperatorsAvsStateDict[signedTaskResponseDigest.OperatorId].Pubkeys.G2Pubkey)
				digestAggregatedOperators.oldSignersOperatorIdsSet[signedTaskResponseDigest.OperatorId] = true
			}

			if _, ok := operatorsAvsStateDict[signedTaskResponseDigest.OperatorId]; ok {
				digestAggregatedOperators.signersAggSigG1.Add(signedTaskResponseDigest.BlsSignature)
				digestAggregatedOperators.signersApkG2.Add(operatorsAvsStateDict[signedTaskResponseDigest.OperatorId].Pubkeys.G2Pubkey)
				digestAggregatedOperators.signersOperatorIdsSet[signedTaskResponseDigest.OperatorId] = true
				for quorumNum, stake := range operatorsAvsStateDict[signedTaskResponseDigest.OperatorId].StakePerQuorum {
					if _, ok := digestAggregatedOperators.signersTotalStakePerQuorum[quorumNum]; !ok {
						// if we haven't seen this quorum before, initialize its signed stake to 0
						// possible if previous operators who sent us signatures were not part of this quorum
						digestAggregatedOperators.signersTotalStakePerQuorum[quorumNum] = big.NewInt(0)
					}
					digestAggregatedOperators.signersTotalStakePerQuorum[quorumNum].Add(digestAggregatedOperators.signersTotalStakePerQuorum[quorumNum], stake)
				}
			}

			// update the aggregatedOperatorsDict. Note that we need to assign the whole struct value at once,
			// because of https://github.com/golang/go/issues/3117
			aggregatedOperatorsDict[signedTaskResponseDigest.TaskResponseDigest] = digestAggregatedOperators
			a.logger.Debug("Task response digest processed", "taskIndex", taskIndex)

		case <-taskResponseDebounceTimer.C:
			if a.finishTask(
				taskIndex,
				taskCreatedBlock,
				quorumNumbers,
				totalStakePerQuorum,
				quorumThresholdPercentagesMap,
				quorumApksG1,
				operatorsAvsStateDict,
				oldOperatorsAvsStateDict,
				aggregatedOperatorsDict,
			) {
				a.logger.Debug("Task goroutine finished for task response digest", "taskIndex", taskIndex)
				return
			}

		// we want to know which operators were active even thou the quorum was not met in time
		case <-taskExpiredTimer.C:
			if len(aggregatedOperatorsDict) == 0 {
				a.aggregatedResponsesC <- BlsAggregationServiceResponse{
					Err:       TaskNotRespondedError,
					TaskIndex: taskIndex,
				}
				return
			}

			// send the task response with the highest totalStake
			var digestAggregatedOperators aggregatedOperators
			var signedTaskResponseDigest types.TaskResponseDigest
			max := big.NewInt(0)
			for digest, aggregatedOperators := range aggregatedOperatorsDict {
				sum := big.NewInt(0)
				for _, val := range aggregatedOperators.signersTotalStakePerQuorum {
					sum.Add(sum, val)
				}
				if sum.Cmp(max) > 0 {
					digestAggregatedOperators = aggregatedOperators
					signedTaskResponseDigest = digest
					max = sum
				}
			}
			blsAggregationServiceResponse, err := a.createResponse(taskIndex, taskCreatedBlock, quorumNumbers, signedTaskResponseDigest, operatorsAvsStateDict, oldOperatorsAvsStateDict, digestAggregatedOperators, quorumApksG1)
			if err != nil {
				a.aggregatedResponsesC <- BlsAggregationServiceResponse{
					Err: err,
				}
			}
			blsAggregationServiceResponse.Err = TaskExpiredError
			a.logger.Info("Task expired, sending response", "total stake", totalStakePerQuorum, "signed stake", digestAggregatedOperators.signersTotalStakePerQuorum)
			a.aggregatedResponsesC <- *blsAggregationServiceResponse
			return
		}
	}
}

func (a *BlsAggregatorService) finishTask(
	taskIndex types.TaskIndex,
	taskCreatedBlock uint32,
	quorumNumbers []types.QuorumNum,
	totalStakePerQuorum map[types.QuorumNum]*big.Int,
	quorumThresholdPercentagesMap map[types.QuorumNum]types.QuorumThresholdPercentage,
	quorumApksG1 []*bls.G1Point,
	operatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
	oldOperatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
	aggregatedOperatorsDict map[types.TaskResponseDigest]aggregatedOperators,
) bool {
	for digest, aggregatedOperators := range aggregatedOperatorsDict {
		if checkIfStakeThresholdsMet(a.logger, aggregatedOperators.signersTotalStakePerQuorum, totalStakePerQuorum, quorumThresholdPercentagesMap) {
			blsAggregationServiceResponse, err := a.createResponse(taskIndex, taskCreatedBlock, quorumNumbers, digest, operatorsAvsStateDict, oldOperatorsAvsStateDict, aggregatedOperators, quorumApksG1)
			if err != nil {
				a.aggregatedResponsesC <- BlsAggregationServiceResponse{
					Err: err,
				}
			}
			a.logger.Info("Task finished, sending response", "total stake", totalStakePerQuorum, "signed stake", aggregatedOperators.signersTotalStakePerQuorum)
			a.aggregatedResponsesC <- *blsAggregationServiceResponse
			return true
		}
	}
	return false
}

// closeTaskGoroutine is run when the goroutine processing taskIndex's task responses ends (for whatever reason)
// it deletes the response channel for taskIndex from a.taskChans
// so that the main thread knows that this task goroutine is no longer running
// and doesn't try to send new signatures to it
func (a *BlsAggregatorService) closeTaskGoroutine(taskIndex types.TaskIndex) {
	a.taskChansMutex.Lock()
	delete(a.signedTaskRespsCs, taskIndex)
	a.taskChansMutex.Unlock()
}

// verifySignature verifies that a signature is valid against the operator pubkey stored in the
// operatorsAvsStateDict for that particular task
// TODO(samlaf): right now we are only checking that the *digest* is signed correctly!!
// we could be sent a signature of any kind of garbage and we would happily aggregate it
// this forces the avs code to verify that the digest is indeed the digest of a valid taskResponse
// we could take taskResponse as an interface{} and have avs code pass us a taskResponseHashFunction
// that we could use to hash and verify the taskResponse itself
func (a *BlsAggregatorService) verifySignature(
	taskIndex types.TaskIndex,
	signedTaskResponseDigest types.SignedTaskResponseDigest,
	operatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
	oldOperatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
) error {
	_, ok := operatorsAvsStateDict[signedTaskResponseDigest.OperatorId]
	_, ok2 := oldOperatorsAvsStateDict[signedTaskResponseDigest.OperatorId]
	if !ok && !ok2 {
		a.logger.Warnf("Operator %#v not found. Skipping message.", signedTaskResponseDigest.OperatorId)
		return OperatorNotPartOfTaskQuorumErrorFn(signedTaskResponseDigest.OperatorId, taskIndex)
	}

	// 0. verify that the msg actually came from the correct operator
	operatorG2Pubkey := operatorsAvsStateDict[signedTaskResponseDigest.OperatorId].Pubkeys.G2Pubkey
	if operatorG2Pubkey == nil {
		a.logger.Fatal("Operator G2 pubkey not found")
	}
	a.logger.Debug("Verifying signed task response digest signature",
		"operatorG2Pubkey", operatorG2Pubkey,
		"taskResponseDigest", signedTaskResponseDigest.TaskResponseDigest,
		"blsSignature", signedTaskResponseDigest.BlsSignature,
	)
	signatureVerified, err := signedTaskResponseDigest.BlsSignature.Verify(operatorG2Pubkey, signedTaskResponseDigest.TaskResponseDigest)
	if err != nil {
		return SignatureVerificationError(err)
	}
	if !signatureVerified {
		return IncorrectSignatureError
	}
	return nil
}

func (a *BlsAggregatorService) createResponse(
	taskIndex types.TaskIndex,
	taskCreatedBlock uint32,
	quorumNumbers []types.QuorumNum,
	signedTaskResponseDigest types.TaskResponseDigest,
	operatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
	oldOperatorsAvsStateDict map[types.OperatorId]types.OperatorAvsState,
	digestAggregatedOperators aggregatedOperators,
	quorumApksG1 []*bls.G1Point,
) (*BlsAggregationServiceResponse, error) {
	nonSignersOperatorIds := []types.OperatorId{}
	nonSignersG1Pubkeys := []*bls.G1Point{}
	nonSignerG1PubkeysForOldState := []*bls.G1Point{}
	
	ks := []types.OperatorId{}
	for operatorId := range operatorsAvsStateDict {
		ks = append(ks, operatorId)
	}
	sort.Slice(ks, func(i, j int) bool {
		a := big.NewInt(0).SetBytes(ks[i][:])
		b := big.NewInt(0).SetBytes(ks[j][:])
		return a.Cmp(b) < 0
	})
	for _, operatorId := range ks {
		operator := operatorsAvsStateDict[operatorId]
		if _, operatorSigned := digestAggregatedOperators.signersOperatorIdsSet[operatorId]; !operatorSigned {
			nonSignersOperatorIds = append(nonSignersOperatorIds, operatorId)
			nonSignersG1Pubkeys = append(nonSignersG1Pubkeys, operator.Pubkeys.G1Pubkey)
		}
	}

	opList := []types.OperatorId{}
	for operatorId := range oldOperatorsAvsStateDict{
		opList = append(opList, operatorId)
	}
	sort.Slice(opList, func(i, j int) bool {
		a := big.NewInt(0).SetBytes(opList[i][:])
		b := big.NewInt(0).SetBytes(opList[j][:])
		return a.Cmp(b) < 0
	})
	for _, operatorId := range opList {
		operator := oldOperatorsAvsStateDict[operatorId]
		if _, operatorSigned := digestAggregatedOperators.oldSignersOperatorIdsSet[operatorId]; !operatorSigned {
			nonSignerG1PubkeysForOldState = append(nonSignerG1PubkeysForOldState, operator.Pubkeys.G1Pubkey)
		}
	}


	indices, err := a.avsRegistryService.GetCheckSignaturesIndices(&bind.CallOpts{}, taskCreatedBlock, quorumNumbers, nonSignersOperatorIds)
	if err != nil {
		a.logger.Error("Failed to get check signatures indices", "err", err)
		return nil, err
	}
	return &BlsAggregationServiceResponse{
		Err:                          nil,
		TaskIndex:                    taskIndex,
		TaskResponseDigest:           signedTaskResponseDigest,
		NonSignersPubkeysG1:          nonSignersG1Pubkeys,
		QuorumApksG1:                 quorumApksG1,
		SignersApkG2:                 digestAggregatedOperators.signersApkG2,
		SignersAggSigG1:              digestAggregatedOperators.signersAggSigG1,
		NonSignerQuorumBitmapIndices: indices.NonSignerQuorumBitmapIndices,
		QuorumApkIndices:             indices.QuorumApkIndices,
		TotalStakeIndices:            indices.TotalStakeIndices,
		NonSignerStakeIndices:        indices.NonSignerStakeIndices,
		OldSignersApkG2:              digestAggregatedOperators.oldSignersApkG2,
		OldSignersAggSigG1:           digestAggregatedOperators.oldSignersAggSigG1,
		NonSignerPubkeysIndicesforOperatorIdsRemovedForOldState:   nonSignerPubkeysIndicesforOperatorIdsRemovedForOldState,
		NonSignerPubkeysAddedForOldState:   nonSignerPubkeysAddedForOldState,
	}, nil
}

// checkIfStakeThresholdsMet checks at least quorumThresholdPercentage of stake
// has signed for each quorum.
func checkIfStakeThresholdsMet(
	logger logging.Logger,
	signedStakePerQuorum map[types.QuorumNum]*big.Int,
	totalStakePerQuorum map[types.QuorumNum]*big.Int,
	quorumThresholdPercentagesMap map[types.QuorumNum]types.QuorumThresholdPercentage,
) bool {
	for quorumNum, quorumThresholdPercentage := range quorumThresholdPercentagesMap {
		signedStakeByQuorum, ok := signedStakePerQuorum[quorumNum]
		if !ok {
			// signedStakePerQuorum not contain the quorum,
			// this case means signedStakePerQuorum has not signed for each quorum.
			// even the total stake for this quorum is zero.
			return false
		}

		totalStakeByQuorum, ok := totalStakePerQuorum[quorumNum]
		if !ok {
			// Note this case should not happend.
			// The `totalStakePerQuorum` is got from the contract, so if we not found the
			// totalStakeByQuorum, that means the code have a bug.
			logger.Errorf("TotalStake not found for quorum %d.", quorumNum)
			return false
		}

		// we check that signedStake >= totalStake * quorumThresholdPercentage / 100
		// to be exact (and do like the contracts), we actually check that
		// signedStake * 100 >= totalStake * quorumThresholdPercentage
		signedStake := big.NewInt(0).Mul(signedStakeByQuorum, big.NewInt(100))
		thresholdStake := big.NewInt(0).Mul(totalStakeByQuorum, big.NewInt(int64(quorumThresholdPercentage)))
		if signedStake.Cmp(thresholdStake) < 0 {
			return false
		}
	}
	return true
}
