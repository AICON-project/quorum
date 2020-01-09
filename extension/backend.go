package extension

import (
	"encoding/base64"
	"errors"
	"github.com/ethereum/go-ethereum/event"
	"sync"

	"github.com/ethereum/go-ethereum/extension/extensionContracts"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
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
				log.Error("Contract extension watcher subscription error", err)
				break

			case foundLog := <-incomingLogs:
				service.mu.Lock()

				tx, _ := service.extClient.TransactionByHash(foundLog.TxHash)
				from, _ := types.QuorumPrivateTxSigner{}.Sender(tx)

				newExtensionEvent, err := extensionContracts.UnpackNewExtensionCreatedLog(foundLog.Data)
				if err != nil {
					log.Error("Error unpacking extension creation log", err.Error())
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
					log.Error("Error writing extension data to file", err.Error())
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
						log.Error("Extension", "Unable to fetch all parties for extension management contract")
						continue
					}
					//Find the extension contract in order to interact with it
					caller, _ := service.managementContractFacade.Caller(newContractExtension.ManagementContractAddress)
					contractCreator, _ := caller.Creator(nil)

					txArgs := ethapi.SendTxArgs{From: contractCreator, PrivateFor: fetchedParties}

					extensionAPI := NewPrivateExtensionAPI(service, service.accountManager, service.ptm)
					_, err = extensionAPI.VoteOnContract(newContractExtension.ManagementContractAddress, true, txArgs)

					if err != nil {
						log.Error("Extension", "initiator vote on management contract failed")
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
				log.Error("Contract cancellation extension watcher subscription error", err)
				return
			case l := <-incomingLogs:
				service.mu.Lock()
				if _, ok := service.currentContracts[l.Address]; ok {
					delete(service.currentContracts, l.Address)
					service.dataHandler.Save(service.currentContracts)
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
				service.mu.Lock()
				extensionEntry, ok := service.currentContracts[l.Address]
				if !ok {
					// we didn't have this management contract, so ignore it
					service.mu.Unlock()
					continue
				}

				//Find the extension contract in order to interact with it
				caller, _ := service.managementContractFacade.Caller(l.Address)
				contractCreator, _ := caller.Creator(nil)

				if !service.accountManager.Exists(contractCreator) {
					log.Warn("Account used to sign extension contract no longer available", "account", contractCreator.Hex())
					service.mu.Unlock()
					continue
				}

				//fetch all the participants and send
				payload := common.BytesToEncryptedPayloadHash(extensionEntry.CreationData)
				fetchedParties, err := service.ptm.GetParticipants(payload)
				if err != nil {
					log.Error("Extension", "Unable to fetch all parties for extension management contract")
					service.mu.Unlock()
					continue
				}

				txArgs, _ := service.accountManager.GenerateTransactOptions(ethapi.SendTxArgs{From: contractCreator, PrivateFor: fetchedParties})

				recipientHash, _ := caller.TargetRecipientPublicKeyHash(&bind.CallOpts{Pending: false})
				decoded, _ := base64.StdEncoding.DecodeString(recipientHash)
				recipient, _ := service.ptm.Receive(decoded)

				//we found the account, so we can send
				contractToExtend, _ := caller.ContractToExtend(nil)
				entireStateData, _ := service.stateFetcher.GetAddressStateFromBlock(l.BlockHash, contractToExtend)

				//send to PTM
				hashOfStateData, _ := service.ptm.Send(entireStateData, "", []string{string(recipient)})
				hashofStateDataBase64 := base64.StdEncoding.EncodeToString(hashOfStateData)

				transactor, _ := service.managementContractFacade.Transactor(l.Address)
				transactor.SetSharedStateHash(txArgs, hashofStateDataBase64)
				service.mu.Unlock()
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
