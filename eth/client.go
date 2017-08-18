/*
Package eth client is the go client for the Livepeer Ethereum smart contract.  Contracts here are generated.
*/
package eth

//go:generate abigen --abi protocol/abi/LivepeerProtocol.abi --pkg contracts --type LivepeerProtocol --out contracts/livepeerProtocol.go --bin protocol/bin/LivepeerProtocol.bin
//go:generate abigen --abi protocol/abi/LivepeerToken.abi --pkg contracts --type LivepeerToken --out contracts/livepeerToken.go --bin protocol/bin/LivepeerToken.bin
//go:generate abigen --abi protocol/abi/BondingManager.abi --pkg contracts --type BondingManager --out contracts/bondingManager.go --bin protocol/bin/BondingManager.bin
//go:generate abigen --abi protocol/abi/JobsManager.abi --pkg contracts --type JobsManager --out contracts/jobsManager.go --bin protocol/bin/JobsManager.bin
//go:generate abigen --abi protocol/abi/RoundsManager.abi --pkg contracts --type RoundsManager --out contracts/roundsManager.go --bin protocol/bin/RoundsManager.bin
//go:generate abigen --abi protocol/abi/IdentityVerifier.abi --pkg contracts --type IdentityVerifier --out contracts/identityVerifier.go --bin protocol/bin/IdentityVerifier.bin
//go:generate abigen --abi protocol/abi/TranscoderPools.abi --pkg contracts --type TranscoderPools --out contracts/transcoderPools.go --bin protocol/bin/TranscoderPools.bin
//go:generate abigen --abi protocol/abi/JobLib.abi --pkg contracts --type JobLib --out contracts/jobLib.go --bin protocol/bin/JobLib.bin
//go:generate abigen --abi protocol/abi/MaxHeap.abi --pkg contracts --type MaxHeap --out contracts/maxHeap.go --bin protocol/bin/MaxHeap.bin
//go:generate abigen --abi protocol/abi/MinHeap.abi --pkg contracts --type MinHeap --out contracts/minHeap.go --bin protocol/bin/MinHeap.bin
//go:generate abigen --abi protocol/abi/Node.abi --pkg contracts --type Node --out contracts/node.go --bin protocol/bin/Node.bin
//go:generate abigen --abi protocol/abi/SafeMath.abi --pkg contracts --type SafeMath --out contracts/safeMath.go --bin protocol/bin/SafeMath.bin
//go:generate abigen --abi protocol/abi/ECRecovery.abi --pkg contracts --type ECRecovery --out contracts/ecRecovery.go --bin protocol/bin/ECRecovery.bin
//go:generate abigen --abi protocol/abi/MerkleProof.abi --pkg contracts --type MerkleProof --out contracts/merkleProof.go --bin protocol/bin/MerkleProof.bin

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/eth/contracts"
)

var ProtocolCyclesPerRound = 2
var ProtocolBlockPerRound = big.NewInt(20)

type LivepeerEthClient interface {
	Backend() *ethclient.Client
	Account() accounts.Account
	SubscribeToJobEvent(ctx context.Context, logsCh chan types.Log) (ethereum.Subscription, error)
	WatchEvent(logsCh <-chan types.Log) (types.Log, error)
	RoundInfo() (*big.Int, *big.Int, *big.Int, error)
	InitializeRound() (<-chan types.Receipt, <-chan error)
	Transcoder(blockRewardCut uint8, feeShare uint8, pricePerSegment *big.Int) (<-chan types.Receipt, <-chan error)
	Bond(amount *big.Int, toAddr common.Address) (<-chan types.Receipt, <-chan error)
	Reward() (<-chan types.Receipt, <-chan error)
	Job(streamId string, transcodingOptions string, maxPricePerSegment *big.Int) (<-chan types.Receipt, <-chan error)
	ClaimWork(jobId *big.Int, segmentRange [2]*big.Int, claimRoot [32]byte) (<-chan types.Receipt, <-chan error)
	Verify(jobId *big.Int, claimId *big.Int, segmentNumber *big.Int, dataHash string, transcodedDataHash string, broadcasterSig []byte, proof []byte) (<-chan types.Receipt, <-chan error)
	Transfer(toAddr common.Address, amount *big.Int) (<-chan types.Receipt, <-chan error)
	CurrentRoundInitialized() (bool, error)
	IsActiveTranscoder() (bool, error)
	TranscoderStake() (*big.Int, error)
	TokenBalance() (*big.Int, error)
}

type Client struct {
	account               accounts.Account
	keyStore              *keystore.KeyStore
	transactOpts          bind.TransactOpts
	backend               *ethclient.Client
	protocolAddr          common.Address
	tokenAddr             common.Address
	bondingManagerAddr    common.Address
	jobsManagerAddr       common.Address
	roundsManagerAddr     common.Address
	protocolSession       *contracts.LivepeerProtocolSession
	tokenSession          *contracts.LivepeerTokenSession
	bondingManagerSession *contracts.BondingManagerSession
	jobsManagerSession    *contracts.JobsManagerSession
	roundsManagerSession  *contracts.RoundsManagerSession

	rpcTimeout   time.Duration
	eventTimeout time.Duration
}

func NewClient(account accounts.Account, passphrase string, datadir string, backend *ethclient.Client, protocolAddr common.Address, tokenAddr common.Address, rpcTimeout time.Duration, eventTimeout time.Duration) (*Client, error) {
	keyStore := keystore.NewKeyStore(filepath.Join(datadir, "keystore"), keystore.StandardScryptN, keystore.StandardScryptP)

	transactOpts, err := NewTransactOptsForAccount(account, passphrase, keyStore)
	if err != nil {
		return nil, err
	}

	token, err := contracts.NewLivepeerToken(tokenAddr, backend)
	if err != nil {
		glog.Errorf("Error creating LivepeerToken: %v", err)
		return nil, err
	}

	protocol, err := contracts.NewLivepeerProtocol(protocolAddr, backend)
	if err != nil {
		glog.Errorf("Error creating LivepeerProtocol: %v", err)
		return nil, err
	}

	client := &Client{
		account:      account,
		keyStore:     keyStore,
		transactOpts: *transactOpts,
		backend:      backend,
		protocolAddr: protocolAddr,
		tokenAddr:    tokenAddr,
		protocolSession: &contracts.LivepeerProtocolSession{
			Contract:     protocol,
			TransactOpts: *transactOpts,
		},
		tokenSession: &contracts.LivepeerTokenSession{
			Contract:     token,
			TransactOpts: *transactOpts,
		},
		rpcTimeout:   rpcTimeout,
		eventTimeout: eventTimeout,
	}

	glog.Infof("Creating client for account %v", transactOpts.From.Hex())

	client.SetManagers()

	return client, nil
}

func (c *Client) SetManagers() error {
	bondingManagerAddr, err := c.protocolSession.Registry(crypto.Keccak256Hash([]byte("BondingManager")))
	if err != nil {
		glog.Errorf("Error getting BondingManager address: %v", err)
		return err
	}

	c.bondingManagerAddr = bondingManagerAddr

	bondingManager, err := contracts.NewBondingManager(bondingManagerAddr, c.backend)
	if err != nil {
		glog.Errorf("Error creating BondingManager: %v", err)
		return err
	}

	c.bondingManagerSession = &contracts.BondingManagerSession{
		Contract:     bondingManager,
		TransactOpts: c.transactOpts,
	}

	jobsManagerAddr, err := c.protocolSession.Registry(crypto.Keccak256Hash([]byte("JobsManager")))
	if err != nil {
		glog.Errorf("Error getting JobsManager address: %v", err)
		return err
	}

	c.jobsManagerAddr = jobsManagerAddr

	jobsManager, err := contracts.NewJobsManager(jobsManagerAddr, c.backend)
	if err != nil {
		glog.Errorf("Error creating JobsManager: %v", err)
		return err
	}

	c.jobsManagerSession = &contracts.JobsManagerSession{
		Contract:     jobsManager,
		TransactOpts: c.transactOpts,
	}

	roundsManagerAddr, err := c.protocolSession.Registry(crypto.Keccak256Hash([]byte("RoundsManager")))
	if err != nil {
		glog.Errorf("Error getting RoundsManager address: %v", err)
		return err
	}

	c.roundsManagerAddr = roundsManagerAddr

	roundsManager, err := contracts.NewRoundsManager(roundsManagerAddr, c.backend)
	if err != nil {
		glog.Errorf("Error creating RoundsManager: %v", err)
		return err
	}

	c.roundsManagerSession = &contracts.RoundsManagerSession{
		Contract:     roundsManager,
		TransactOpts: c.transactOpts,
	}

	glog.Infof("Client: [LivepeerProtocol: %v LivepeerToken: %v BondingManager: %v JobsManager: %v RoundsManager: %v]", c.protocolAddr.Hex(), c.tokenAddr.Hex(), bondingManagerAddr.Hex(), jobsManagerAddr.Hex(), roundsManagerAddr.Hex())

	return nil
}

func (c *Client) Backend() *ethclient.Client {
	return c.backend
}

func (c *Client) Account() accounts.Account {
	return c.account
}

func NewTransactOptsForAccount(account accounts.Account, passphrase string, keyStore *keystore.KeyStore) (*bind.TransactOpts, error) {
	keyjson, err := keyStore.Export(account, passphrase, passphrase)

	if err != nil {
		return nil, err
	}

	transactOpts, err := bind.NewTransactor(bytes.NewReader(keyjson), passphrase)

	if err != nil {
		return nil, err
	}

	return transactOpts, err
}

func (c *Client) SubscribeToJobEvent(ctx context.Context, logsCh chan types.Log) (ethereum.Subscription, error) {
	abiJSON, err := abi.JSON(strings.NewReader(contracts.JobsManagerABI))
	if err != nil {
		glog.Errorf("Error decoding ABI into JSON: %v", err)
		return nil, err
	}

	q := ethereum.FilterQuery{
		Addresses: []common.Address{c.jobsManagerAddr},
		Topics:    [][]common.Hash{[]common.Hash{abiJSON.Events["NewJob"].Id()}, []common.Hash{common.BytesToHash(common.LeftPadBytes(c.account.Address[:], 32))}},
	}

	return c.backend.SubscribeFilterLogs(ctx, q, logsCh)
}

func (c *Client) WatchEvent(logsCh <-chan types.Log) (types.Log, error) {
	var (
		timer = time.NewTimer(c.eventTimeout)
	)

	for {
		select {
		case log := <-logsCh:
			if !log.Removed {
				return log, nil
			}
		case <-timer.C:
			err := fmt.Errorf("watchEvent timed out")

			glog.Errorf(err.Error())
			return types.Log{}, err
		}
	}
}

func (c *Client) RoundInfo() (*big.Int, *big.Int, *big.Int, error) {
	cr, err := c.roundsManagerSession.CurrentRound()

	if err != nil {
		glog.Errorf("Error getting current round: %v", err)
		return nil, nil, nil, err
	}

	crsb, err := c.roundsManagerSession.CurrentRoundStartBlock()

	if err != nil {
		glog.Errorf("Error getting current round start block: %v", err)
		return nil, nil, nil, err
	}

	ctx, _ := context.WithTimeout(context.Background(), c.rpcTimeout)

	block, err := c.backend.BlockByNumber(ctx, nil)

	if err != nil {
		glog.Errorf("Error getting latest block number: %v", err)
		return nil, nil, nil, err
	}

	return cr, crsb, block.Number(), nil
}

func (c *Client) CurrentRoundInitialized() (bool, error) {
	initialized, err := c.roundsManagerSession.CurrentRoundInitialized()

	if err != nil {
		glog.Errorf("Error checking if current round initialized: %v", err)
		return false, err
	}

	return initialized, nil
}

// TRANSACTIONS

func (c *Client) InitializeRound() (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.roundsManagerSession.InitializeRound()
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted tx %v. Initialize round", c.account.Address.Hex(), tx.Hash().Hex())

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) Transcoder(blockRewardCut uint8, feeShare uint8, pricePerSegment *big.Int) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.bondingManagerSession.Transcoder(blockRewardCut, feeShare, pricePerSegment)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted tx %v. Register as transcoder", c.account.Address.Hex(), tx.Hash().Hex())

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
			return
		}

		outRes <- *receipt

		return
	}()

	return outRes, outErr
}

func (c *Client) Bond(amount *big.Int, toAddr common.Address) (<-chan types.Receipt, <-chan error) {
	inRes, inErr := c.Approve(c.bondingManagerAddr, amount)

	timer := time.NewTimer(c.eventTimeout)
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		select {
		case log := <-inRes:
			if !log.Removed {
				tx, err := c.bondingManagerSession.Bond(amount, toAddr)
				if err != nil {
					outErr <- err
					return
				}

				glog.Infof("[%v] Submitted tx %v. Bond %v LPTU to %v", c.account.Address.Hex(), tx.Hash().Hex(), amount, toAddr.Hex())

				receipt, err := c.WaitForReceipt(tx)
				if err != nil {
					outErr <- err
				} else {
					outRes <- *receipt
				}

				return
			}
		case err := <-inErr:
			outErr <- err
			return
		case <-timer.C:
			outErr <- fmt.Errorf("Event subscription timed out")
			return
		}
	}()

	return outRes, outErr
}

func (c *Client) Reward() (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.bondingManagerSession.Reward()
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted tx %v. Called reward", c.account.Address.Hex(), tx.Hash().Hex())

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) Deposit(amount *big.Int) (<-chan types.Receipt, <-chan error) {
	inRes, inErr := c.Approve(c.jobsManagerAddr, amount)

	timer := time.NewTimer(c.eventTimeout)
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		select {
		case log := <-inRes:
			if !log.Removed {
				tx, err := c.jobsManagerSession.Deposit(amount)
				if err != nil {
					outErr <- err
					return
				}

				glog.Infof("[%v] Submitted tx %v. Deposited %v LPTU", c.account.Address.Hex(), tx.Hash().Hex(), amount)

				receipt, err := c.WaitForReceipt(tx)
				if err != nil {
					outErr <- err
				} else {
					outRes <- *receipt
				}

				return
			}
		case err := <-inErr:
			outErr <- err
			return
		case <-timer.C:
			outErr <- fmt.Errorf("Event subscription timed out")
			return
		}
	}()

	return outRes, outErr
}

func (c *Client) Job(streamId string, transcodingOptions string, maxPricePerSegment *big.Int) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.jobsManagerSession.Job(streamId, transcodingOptions, maxPricePerSegment)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted tx %v. Creating job for stream id %v", c.account.Address.Hex(), tx.Hash().Hex(), streamId)

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) ClaimWork(jobId *big.Int, segmentRange [2]*big.Int, claimRoot [32]byte) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.jobsManagerSession.ClaimWork(jobId, segmentRange, claimRoot)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted transaction %v. Claimed work for segments %v - %v", c.account.Address.Hex(), tx.Hash().Hex(), segmentRange[0], segmentRange[1])

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) Verify(jobId *big.Int, claimId *big.Int, segmentNumber *big.Int, dataHash string, transcodedDataHash string, broadcasterSig []byte, proof []byte) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.jobsManagerSession.Verify(jobId, claimId, segmentNumber, dataHash, transcodedDataHash, broadcasterSig, proof)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted tx %v. Verify segment %v in claim %v", c.account.Address.Hex(), tx.Hash().Hex(), segmentNumber, claimId)

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) DistributeFees(jobId *big.Int, claimId *big.Int) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.jobsManagerSession.DistributeFees(jobId, claimId)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted transaction %v. Distributed fees for job %v claim %v", c.account.Address.Hex(), tx.Hash().Hex(), jobId, claimId)

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) Transfer(toAddr common.Address, amount *big.Int) (<-chan types.Receipt, <-chan error) {
	outRes := make(chan types.Receipt)
	outErr := make(chan error)

	go func() {
		defer close(outRes)
		defer close(outErr)

		tx, err := c.tokenSession.Transfer(toAddr, amount)
		if err != nil {
			outErr <- err
			return
		}

		glog.Infof("[%v] Submitted transaction %v. Transfer %v LPTU to %v", c.account.Address.Hex(), tx.Hash().Hex(), amount, toAddr.Hex())

		receipt, err := c.WaitForReceipt(tx)
		if err != nil {
			outErr <- err
		} else {
			outRes <- *receipt
		}

		return
	}()

	return outRes, outErr
}

func (c *Client) Approve(toAddr common.Address, amount *big.Int) (chan types.Log, chan error) {
	outRes := make(chan types.Log)
	outErr := make(chan error)

	logsCh, sub, err := c.SubscribeToApproval()
	if err != nil {
		outErr <- err

		close(outRes)
		close(outErr)
	}

	_, err = c.tokenSession.Approve(toAddr, amount)
	if err != nil {
		outErr <- err

		close(outRes)
		close(outErr)
	}

	go func() {
		log := <-logsCh

		close(logsCh)
		sub.Unsubscribe()

		outRes <- log

		close(outRes)
		close(outErr)
	}()

	return outRes, outErr
}

func (c *Client) SubscribeToApproval() (chan types.Log, ethereum.Subscription, error) {
	logsCh := make(chan types.Log)

	abiJSON, err := abi.JSON(strings.NewReader(contracts.LivepeerTokenABI))
	if err != nil {
		return nil, nil, err
	}

	q := ethereum.FilterQuery{
		Addresses: []common.Address{c.tokenAddr},
		Topics:    [][]common.Hash{[]common.Hash{abiJSON.Events["Approval"].Id()}, []common.Hash{common.BytesToHash(common.LeftPadBytes(c.account.Address[:], 32))}},
	}

	ctx, _ := context.WithTimeout(context.Background(), c.rpcTimeout)

	sub, err := c.backend.SubscribeFilterLogs(ctx, q, logsCh)
	if err != nil {
		return nil, nil, err
	}

	return logsCh, sub, nil
}

func (c *Client) IsActiveTranscoder() (bool, error) {
	return c.bondingManagerSession.IsActiveTranscoder(c.account.Address)
}

func (c *Client) TranscoderBond() (*big.Int, error) {
	transcoder, err := c.bondingManagerSession.Transcoders(c.account.Address)
	if err != nil {
		return nil, err
	}

	return transcoder.BondedAmount, nil
}

func (c *Client) TranscoderStake() (*big.Int, error) {
	return c.bondingManagerSession.TranscoderTotalStake(c.account.Address)
}

func (c *Client) TranscoderStatus() (uint8, error) {
	return c.bondingManagerSession.TranscoderStatus(c.account.Address)
}

func (c *Client) DelegatorStake() (*big.Int, error) {
	return c.bondingManagerSession.DelegatorStake(c.account.Address)
}

func (c *Client) LastRewardRound() (*big.Int, error) {
	transcoderDetails, err := c.bondingManagerSession.Transcoders(c.account.Address)

	if err != nil {
		glog.Errorf("Error getting transcoder details: %v", err)
		return nil, err
	}

	return transcoderDetails.LastRewardRound, nil
}

func (c *Client) ProtocolTimeParams() (*struct {
	RoundLength        *big.Int
	JobEndingPeriod    *big.Int
	VerificationPeriod *big.Int
	SlashingPeriod     *big.Int
	UnbondingPeriod    uint64
}, error) {
	roundLength, err := c.roundsManagerSession.RoundLength()
	if err != nil {
		return nil, err
	}

	jobEndingPeriod, err := c.jobsManagerSession.JobEndingPeriod()
	if err != nil {
		return nil, err
	}

	verificationPeriod, err := c.jobsManagerSession.VerificationPeriod()
	if err != nil {
		return nil, err
	}

	slashingPeriod, err := c.jobsManagerSession.SlashingPeriod()
	if err != nil {
		return nil, err
	}

	unbondingPeriod, err := c.bondingManagerSession.UnbondingPeriod()
	if err != nil {
		return nil, err
	}

	timingParams := &struct {
		RoundLength        *big.Int
		JobEndingPeriod    *big.Int
		VerificationPeriod *big.Int
		SlashingPeriod     *big.Int
		UnbondingPeriod    uint64
	}{
		roundLength,
		jobEndingPeriod,
		verificationPeriod,
		slashingPeriod,
		unbondingPeriod,
	}

	return timingParams, nil
}

func (c *Client) GetJob(jobID *big.Int) (struct {
	JobId              *big.Int
	StreamId           string
	TranscodingOptions string
	MaxPricePerSegment *big.Int
	PricePerSegment    *big.Int
	BroadcasterAddress common.Address
	TranscoderAddress  common.Address
	EndBlock           *big.Int
	Escrow             *big.Int
}, error) {
	return c.jobsManagerSession.Jobs(jobID)
}

// Token methods

func (c *Client) TokenBalance() (*big.Int, error) {
	return c.tokenSession.BalanceOf(c.account.Address)
}

func (c *Client) SignSegmentHash(passphrase string, hash []byte) ([]byte, error) {
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", 32, hash)
	signHash := crypto.Keccak256([]byte(msg))

	sig, err := c.keyStore.SignHashWithPassphrase(c.account, passphrase, signHash)
	if err != nil {
		glog.Errorf("Error signing segment hash: %v", err)
		return nil, err
	}

	return sig, nil
}

func (c *Client) WaitForReceipt(tx *types.Transaction) (*types.Receipt, error) {
	for time.Since(time.Now()) < c.eventTimeout {
		ctx, _ := context.WithTimeout(context.Background(), c.rpcTimeout)

		receipt, err := c.backend.TransactionReceipt(ctx, tx.Hash())
		if err != nil && err != ethereum.NotFound {
			return nil, err
		}

		if receipt != nil {
			if tx.Gas().Cmp(receipt.GasUsed) == 0 {
				return nil, fmt.Errorf("Tx %v threw", tx.Hash().Hex())
			} else {
				return receipt, nil
			}
		}

		time.Sleep(time.Second)
	}

	return nil, fmt.Errorf("Tx %v timed out", tx.Hash().Hex())
}
