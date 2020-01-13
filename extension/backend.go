package extension

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/extension/extensionContracts"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/private"
	"github.com/ethereum/go-ethereum/rpc"
)

type PrivacyService struct {
	ptm private.PrivateTransactionManager

	stateFetcher             *StateFetcher
	accountManager           IAccountManager
	dataHandler              DataHandler
	managementContractFacade ManagementContractFacade
	extClient                Client
	stopFeed                 event.Feed

	mu               sync.Mutex
	currentContracts map[common.Address]*ExtensionContract
}

// to signal all watches when service is stopped
type stopEvent struct {
}

func (service *PrivacyService) subscribeStopEvent() (chan stopEvent, event.Subscription) {
	c := make(chan stopEvent)
	s := service.stopFeed.Subscribe(c)
	return c, s
}

func New(ptm private.PrivateTransactionManager, manager IAccountManager, handler DataHandler, fetcher *StateFetcher) (*PrivacyService, error) {
	service := &PrivacyService{
		currentContracts: make(map[common.Address]*ExtensionContract),
		ptm:              ptm,
		dataHandler:      handler,
		stateFetcher:     fetcher,
		accountManager:   manager,
	}

	var err error
	service.currentContracts, err = service.dataHandler.Load()
	if err != nil {
		return nil, errors.New("could not load existing extension contracts: " + err.Error())
	}

	return service, nil
}

func (service *PrivacyService) initialise(node *node.Node, thirdpartyunixfile string) {
	service.mu.Lock()
	defer service.mu.Unlock()

	rpcClient, err := node.Attach()
	if err != nil {
		panic("extension: could not connect to ethereum client rpc")
	}

	client, _ := ethclient.NewClient(rpcClient).WithIPCPrivateTransactionManager(thirdpartyunixfile)
	service.managementContractFacade = NewManagementContractFacade(client)
	service.extClient = NewInProcessClient(client)

	for _, f := range []func() error{
		service.watchForNewContracts,       // watch for new extension contract creation event
		service.watchForCancelledContracts, // watch for extension contract cancellation event
		service.watchForCompletionEvents,   // watch for extension contract voting complete event
	} {
		if err := f(); err != nil {
			log.Error("")
		}
	}

}

func (service *PrivacyService) watchForNewContracts() error {
	incomingLogs, subscription, err := service.extClient.SubscribeToLogs(newExtensionQuery)

	if err != nil {
		return err
	}

	go func() {
		stopChan, stopSubscription := service.subscribeStopEvent()
		defer stopSubscription.Unsubscribe()
		for {
			select {
			case err := <-subscription.Err():
				log.Error("Contract extension watcher subscription error", "error", err)
				break

			case foundLog := <-incomingLogs:
				service.mu.Lock()

				tx, _ := service.extClient.TransactionByHash(foundLog.TxHash)
				from, _ := types.QuorumPrivateTxSigner{}.Sender(tx)

				newExtensionEvent, err := extensionContracts.UnpackNewExtensionCreatedLog(foundLog.Data)
				if err != nil {
					log.Error("Error unpacking extension creation log", "error", err)
					log.Debug("Errored log", foundLog)
					service.mu.Unlock()
					continue
				}

				newContractExtension := ExtensionContract{
					Address:                   newExtensionEvent.ToExtend,
					Initiator:                 from,
					ManagementContractAddress: foundLog.Address,
					CreationData:              tx.Data(),
				}

				service.currentContracts[foundLog.Address] = &newContractExtension
				err = service.dataHandler.Save(service.currentContracts)
				if err != nil {
					log.Error("Error writing extension data to file", "error", err)
					service.mu.Unlock()
					continue
				}
				service.mu.Unlock()

				// if party is sender then complete self voting
				data := common.BytesToEncryptedPayloadHash(newContractExtension.CreationData)
				isSender, _ := service.ptm.IsSender(data)

				if isSender {
					fetchedParties, err := service.ptm.GetParticipants(data)
					if err != nil {
						log.Error("Extension: unable to fetch all parties for extension management contract", "error", err)
						continue
					}
					//Find the extension contract in order to interact with it
					caller, _ := service.managementContractFacade.Caller(newContractExtension.ManagementContractAddress)
					contractCreator, _ := caller.Creator(nil)

					txArgs := ethapi.SendTxArgs{From: contractCreator, PrivateFor: fetchedParties}

					extensionAPI := NewPrivateExtensionAPI(service, service.accountManager, service.ptm)
					_, err = extensionAPI.ApproveContractExtension(newContractExtension.ManagementContractAddress, true, txArgs)

					if err != nil {
						log.Error("Extension: initiator vote on management contract failed", "error", err)
					}
				}

			case <-stopChan:
				return
			}
		}
	}()

	return nil
}

func (service *PrivacyService) watchForCancelledContracts() error {
	incomingLogs, subscription, err := service.extClient.SubscribeToLogs(finishedExtensionQuery)

	if err != nil {
		return err
	}

	go func() {
		stopChan, stopSubscription := service.subscribeStopEvent()
		defer stopSubscription.Unsubscribe()
		for {
			select {
			case err := <-subscription.Err():
				log.Error("Contract cancellation extension watcher subscription error", "error", err)
				return
			case l := <-incomingLogs:
				service.mu.Lock()
				if _, ok := service.currentContracts[l.Address]; ok {
					delete(service.currentContracts, l.Address)
					if err := service.dataHandler.Save(service.currentContracts); err != nil {
						log.Error("Faile to store list of contracts being extended", "error", err)
					}
				}
				service.mu.Unlock()
			case <-stopChan:
				return
			}
		}

	}()

	return nil
}

func (service *PrivacyService) watchForCompletionEvents() error {
	incomingLogs, _, err := service.extClient.SubscribeToLogs(canPerformStateShareQuery)

	if err != nil {
		return err
	}

	go func() {
		stopChan, stopSubscription := service.subscribeStopEvent()
		defer stopSubscription.Unsubscribe()
		for {
			select {
			case l := <-incomingLogs:
				log.Debug("[SOS] Receieved a completion event", "address", l.Address.Hex(), "blockNumber", l.BlockNumber)
				service.mu.Lock()
				func() {
					defer func() {
						service.mu.Unlock()
					}()
					extensionEntry, ok := service.currentContracts[l.Address]
					if !ok {
						// we didn't have this management contract, so ignore it
						log.Debug("[SOS] this node doesn't participate in the contract extender", "address", l.Address.Hex())
						return
					}

					//Find the extension contract in order to interact with it
					caller, err := service.managementContractFacade.Caller(l.Address)
					if err != nil {
						log.Error("service.managementContractFacade.Caller", "address", l.Address.Hex(), "error", err)
						return
					}
					contractCreator, err := caller.Creator(nil)
					if err != nil {
						log.Error("[contract] caller.Creator", "error", err)
						return
					}
					log.Debug("[SOS] check if this node has the account that created the contract extender", "account", contractCreator)
					if !service.accountManager.Exists(contractCreator) {
						log.Warn("Account used to sign extension contract no longer available", "account", contractCreator.Hex())
						return
					}

					//fetch all the participants and send
					payload := common.BytesToEncryptedPayloadHash(extensionEntry.CreationData)
					fetchedParties, err := service.ptm.GetParticipants(payload)
					if err != nil {
						log.Error("Extension: Unable to fetch all parties for extension management contract", "error", err)
						return
					}
					log.Debug("[SOS] able to fetch all parties", "parties", fetchedParties)

					txArgs, err := service.accountManager.GenerateTransactOptions(ethapi.SendTxArgs{From: contractCreator, PrivateFor: fetchedParties})
					if err != nil {
						log.Error("service.accountManager.GenerateTransactOptions", "error", err, "contractCreator", contractCreator.Hex(), "privateFor", fetchedParties)
						return
					}

					recipientHash, err := caller.TargetRecipientPublicKeyHash(&bind.CallOpts{Pending: false})
					if err != nil {
						log.Error("[contract] caller.TargetRecipientPublicKeyHash", "error", err)
						return
					}
					decoded, err := base64.StdEncoding.DecodeString(recipientHash)
					if err != nil {
						log.Error("base64.StdEncoding.DecodeString", "recipientHash", recipientHash, "error", err)
						return
					}
					recipient, err := service.ptm.Receive(decoded)
					if err != nil {
						log.Error("[ptm] service.ptm.Receive", "decodedInHex", hex.EncodeToString(decoded[:]), "error", err)
						return
					}
					log.Debug("[SOS] able to retrieve the public key of the new party", "publicKey", string(recipient))

					//we found the account, so we can send
					contractToExtend, err := caller.ContractToExtend(nil)
					if err != nil {
						log.Error("[contract] caller.ContractToExtend", "error", err)
						return
					}
					log.Debug("[SOS] dump current state", "block", l.BlockHash, "contract", contractToExtend.Hex())
					entireStateData, err := service.stateFetcher.GetAddressStateFromBlock(l.BlockHash, contractToExtend)
					if err != nil {
						log.Error("[state] service.stateFetcher.GetAddressStateFromBlock", "block", l.BlockHash.Hex(), "contract", contractToExtend.Hex(), "error", err)
						return
					}

					log.Debug("[SOS] send the state dump to the new recipient", "recipient", string(recipient))
					//send to PTM
					hashOfStateData, err := service.ptm.Send(entireStateData, "", []string{string(recipient)})
					if err != nil {
						log.Error("[ptm] service.ptm.Send", "stateDataInHex", hex.EncodeToString(entireStateData[:]), "recipient", string(recipient), "error", err)
						return
					}
					hashofStateDataBase64 := base64.StdEncoding.EncodeToString(hashOfStateData)

					transactor, err := service.managementContractFacade.Transactor(l.Address)
					if err != nil {
						log.Error("service.managementContractFacade.Transactor", "address", l.Address.Hex(), "error", err)
						return
					}
					log.Debug("[SOS] store the encrypted payload hash of dump state", "contract", l.Address.Hex())
					if tx, err := transactor.SetSharedStateHash(txArgs, hashofStateDataBase64); err != nil {
						log.Error("[contract] transactor.SetSharedStateHash", "error", err, "hashOfStateInBase64", hashofStateDataBase64)
					} else {
						log.Debug("[SOS] transaction carrying shared state", "txhash", tx.Hash(), "private", tx.IsPrivate())
					}
				}()
			case <-stopChan:
				return
			}
		}

	}()
	return nil
}

// node.Service interface methods:
func (service *PrivacyService) Protocols() []p2p.Protocol {
	return []p2p.Protocol{}
}

func (service *PrivacyService) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "quorumExtension",
			Version:   "1.0",
			Service:   NewPrivateExtensionAPI(service, service.accountManager, service.ptm),
			Public:    true,
		},
	}
}

func (service *PrivacyService) Start(p2pServer *p2p.Server) error {
	log.Debug("extension service: starting")
	return nil
}

func (service *PrivacyService) Stop() error {
	log.Info("extension service: stopping")
	service.stopFeed.Send(stopEvent{})
	log.Info("extension service: stopped")
	return nil
}
