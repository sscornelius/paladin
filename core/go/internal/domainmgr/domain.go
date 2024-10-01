/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package domainmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-signer/pkg/abi"
	"github.com/hyperledger/firefly-signer/pkg/eip712"
	"github.com/hyperledger/firefly-signer/pkg/ethsigner"
	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/hyperledger/firefly-signer/pkg/secp256k1"
	"github.com/kaleido-io/paladin/core/internal/components"
	"github.com/kaleido-io/paladin/core/internal/msgs"
	"github.com/kaleido-io/paladin/core/internal/statestore"
	"github.com/kaleido-io/paladin/core/pkg/blockindexer"
	"github.com/kaleido-io/paladin/core/pkg/config"
	"gorm.io/gorm"

	"github.com/kaleido-io/paladin/toolkit/pkg/algorithms"
	"github.com/kaleido-io/paladin/toolkit/pkg/log"
	"github.com/kaleido-io/paladin/toolkit/pkg/prototk"
	"github.com/kaleido-io/paladin/toolkit/pkg/query"
	"github.com/kaleido-io/paladin/toolkit/pkg/retry"
	"github.com/kaleido-io/paladin/toolkit/pkg/signpayloads"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"
)

type domain struct {
	ctx       context.Context
	cancelCtx context.CancelFunc

	conf            *config.DomainConfig
	dm              *domainManager
	name            string
	api             components.DomainManagerToDomain
	registryAddress *tktypes.EthAddress

	stateLock          sync.Mutex
	initialized        atomic.Bool
	initRetry          *retry.Retry
	config             *prototk.DomainConfig
	schemasBySignature map[string]statestore.Schema
	schemasByID        map[string]statestore.Schema
	eventStream        *blockindexer.EventStream

	initError atomic.Pointer[error]
	initDone  chan struct{}
}

func (dm *domainManager) newDomain(name string, conf *config.DomainConfig, toDomain components.DomainManagerToDomain) *domain {
	d := &domain{
		dm:              dm,
		conf:            conf,
		initRetry:       retry.NewRetryIndefinite(&conf.Init.Retry),
		name:            name,
		api:             toDomain,
		initDone:        make(chan struct{}),
		registryAddress: tktypes.MustEthAddress(conf.RegistryAddress), // check earlier in startup

		schemasByID:        make(map[string]statestore.Schema),
		schemasBySignature: make(map[string]statestore.Schema),
	}
	log.L(dm.bgCtx).Debugf("Domain %s configured. Config: %s", name, tktypes.JSONString(conf.Config))
	d.ctx, d.cancelCtx = context.WithCancel(log.WithLogField(dm.bgCtx, "domain", d.name))
	return d
}

func (d *domain) processDomainConfig(confRes *prototk.ConfigureDomainResponse) (*prototk.InitDomainRequest, error) {
	d.stateLock.Lock()
	defer d.stateLock.Unlock()

	// Parse all the schemas
	d.config = confRes.DomainConfig
	if d.config.BaseLedgerSubmitConfig == nil {
		return nil, i18n.NewError(d.ctx, msgs.MsgDomainBaseLedgerSubmitInvalid)
	}
	abiSchemas := make([]*abi.Parameter, len(d.config.AbiStateSchemasJson))
	for i, schemaJSON := range d.config.AbiStateSchemasJson {
		if err := json.Unmarshal([]byte(schemaJSON), &abiSchemas[i]); err != nil {
			return nil, i18n.WrapError(d.ctx, err, msgs.MsgDomainInvalidSchema, i)
		}
	}

	// Ensure all the schemas are recorded to the DB
	var schemas []statestore.Schema
	if len(abiSchemas) > 0 {
		var err error
		schemas, err = d.dm.stateStore.EnsureABISchemas(d.ctx, d.name, abiSchemas)
		if err != nil {
			return nil, err
		}
	}

	// Build the schema IDs to send back in the init
	schemasProto := make([]*prototk.StateSchema, len(schemas))
	for i, s := range schemas {
		schemaID := s.IDString()
		d.schemasByID[schemaID] = s
		d.schemasBySignature[s.Signature()] = s
		schemasProto[i] = &prototk.StateSchema{
			Id:        schemaID,
			Signature: s.Signature(),
		}
	}

	stream := &blockindexer.EventStream{
		Type: blockindexer.EventStreamTypeInternal.Enum(),
		Sources: []blockindexer.EventStreamSource{
			{ABI: iPaladinContractRegistryABI, Address: d.registryAddress},
		},
	}

	if d.config.AbiEventsJson != "" {
		// Parse the events ABI
		var eventsABI abi.ABI
		if err := json.Unmarshal([]byte(d.config.AbiEventsJson), &eventsABI); err != nil {
			return nil, i18n.WrapError(d.ctx, err, msgs.MsgDomainInvalidEvents)
		}
		stream.Sources = append(stream.Sources, blockindexer.EventStreamSource{ABI: eventsABI})
	}

	// We build a stream name in a way assured to result in a new stream if the ABI changes
	// TODO: clean up defunct streams
	var abiHashes []byte
	for _, s := range stream.Sources {
		hash, err := tktypes.ABISolDefinitionHash(d.ctx, s.ABI)
		if err != nil {
			return nil, err
		}
		abiHashes = append(abiHashes, hash[:]...)
	}
	streamHash := tktypes.Bytes32Keccak(abiHashes)
	stream.Name = fmt.Sprintf("domain_%s_%s", d.name, streamHash)

	// Create the event stream
	var err error
	d.eventStream, err = d.dm.blockIndexer.AddEventStream(d.ctx, &blockindexer.InternalEventStream{
		Definition: stream,
		Handler:    d.handleEventBatch,
	})
	if err != nil {
		return nil, err
	}

	return &prototk.InitDomainRequest{
		AbiStateSchemas: schemasProto,
	}, nil
}

func (d *domain) init() {
	defer close(d.initDone)

	// We block retrying each part of init until we succeed, or are cancelled
	// (which the plugin manager will do if the domain disconnects)
	err := d.initRetry.Do(d.ctx, func(attempt int) (bool, error) {

		// Send the configuration to the domain for processing
		confRes, err := d.api.ConfigureDomain(d.ctx, &prototk.ConfigureDomainRequest{
			Name:                    d.name,
			RegistryContractAddress: d.RegistryAddress().String(),
			ChainId:                 d.dm.ethClientFactory.ChainID(),
			ConfigJson:              tktypes.JSONString(d.conf.Config).String(),
		})
		if err != nil {
			return true, err
		}

		// Process the configuration, so we can move onto init
		initReq, err := d.processDomainConfig(confRes)
		if err != nil {
			return true, err
		}

		// Complete the initialization
		_, err = d.api.InitDomain(d.ctx, initReq)
		return true, err
	})
	if err != nil {
		log.L(d.ctx).Debugf("domain initialization cancelled before completion: %s", err)
		d.initError.Store(&err)
	} else {
		log.L(d.ctx).Debugf("domain initialization complete")
		d.dm.setDomainAddress(d)
		d.initialized.Store(true)
		// Inform the plugin manager callback
		d.api.Initialized()
	}
}

func (d *domain) checkInit(ctx context.Context) error {
	if !d.initialized.Load() {
		return i18n.NewError(ctx, msgs.MsgDomainNotInitialized)
	}
	return nil
}

func (d *domain) Initialized() bool {
	return d.initialized.Load()
}

func (d *domain) Name() string {
	return d.name
}

func (d *domain) RegistryAddress() *tktypes.EthAddress {
	return d.registryAddress
}

func (d *domain) Configuration() *prototk.DomainConfig {
	return d.config
}

// Domain callback to query the state store
func (d *domain) FindAvailableStates(ctx context.Context, req *prototk.FindAvailableStatesRequest) (*prototk.FindAvailableStatesResponse, error) {
	if err := d.checkInit(ctx); err != nil {
		return nil, err
	}

	var query query.QueryJSON
	err := json.Unmarshal([]byte(req.QueryJson), &query)
	if err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgDomainInvalidQueryJSON)
	}
	addr, err := tktypes.ParseEthAddress(req.ContractAddress)
	if err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgDomainErrorParsingAddress)
	}

	var states []*statestore.State
	err = d.dm.stateStore.RunInDomainContext(d.name, *addr, func(ctx context.Context, dsi statestore.DomainStateInterface) (err error) {
		if req.UseNullifiers != nil && *req.UseNullifiers {
			states, err = dsi.FindAvailableNullifiers(req.SchemaId, &query)
		} else {
			states, err = dsi.FindAvailableStates(req.SchemaId, &query)
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	pbStates := make([]*prototk.StoredState, len(states))
	for i, s := range states {
		pbStates[i] = &prototk.StoredState{
			Id:       s.ID.String(),
			SchemaId: s.Schema.String(),
			StoredAt: s.Created.UnixNano(),
			DataJson: string(s.Data),
		}
		if s.Locked != nil {
			pbStates[i].Lock = &prototk.StateLock{
				Transaction: s.Locked.Transaction.String(),
				Creating:    s.Locked.Creating,
				Spending:    s.Locked.Spending,
			}
		}
	}
	return &prototk.FindAvailableStatesResponse{
		States: pbStates,
	}, nil

}

func (d *domain) EncodeData(ctx context.Context, encRequest *prototk.EncodeDataRequest) (*prototk.EncodeDataResponse, error) {
	var abiData []byte
	switch encRequest.EncodingType {
	case prototk.EncodeDataRequest_FUNCTION_CALL_DATA:
		var entry *abi.Entry
		err := json.Unmarshal([]byte(encRequest.Definition), &entry)
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingRequestEntryInvalid)
		}
		abiData, err = entry.EncodeCallDataJSONCtx(ctx, []byte(encRequest.Body))
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingRequestEncodingFail)
		}
	case prototk.EncodeDataRequest_TUPLE:
		var param *abi.Parameter
		err := json.Unmarshal([]byte(encRequest.Definition), &param)
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingRequestEntryInvalid)
		}
		abiData, err = param.Components.EncodeABIDataJSONCtx(ctx, []byte(encRequest.Body))
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingRequestEncodingFail)
		}
	case prototk.EncodeDataRequest_ETH_TRANSACTION:
		var tx *ethsigner.Transaction
		err := json.Unmarshal([]byte(encRequest.Body), &tx)
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingRequestEntryInvalid)
		}
		// We only support EIP-155 and EIP-1559 as they include the ChainID in the payload
		switch encRequest.Definition {
		case "", "eip1559", "eip-1559": // default
			abiData = tx.SignaturePayloadEIP1559(d.dm.ethClientFactory.ChainID()).Bytes()
		case "eip155", "eip-155":
			abiData = tx.SignaturePayloadLegacyEIP155(d.dm.ethClientFactory.ChainID()).Bytes()
		default:
			return nil, i18n.NewError(ctx, msgs.MsgDomainABIEncodingRequestInvalidType, encRequest.Definition)
		}
	case prototk.EncodeDataRequest_TYPED_DATA_V4:
		var tdv4 *eip712.TypedData
		err := json.Unmarshal([]byte(encRequest.Body), &tdv4)
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingTypedDataInvalid)
		}
		abiData, err = eip712.EncodeTypedDataV4(ctx, tdv4)
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIEncodingTypedDataFail)
		}
	default:
		return nil, i18n.NewError(ctx, msgs.MsgDomainABIEncodingRequestInvalidType, encRequest.EncodingType)
	}
	return &prototk.EncodeDataResponse{
		Data: abiData,
	}, nil
}

func (d *domain) RecoverSigner(ctx context.Context, recoverRequest *prototk.RecoverSignerRequest) (_ *prototk.RecoverSignerResponse, err error) {
	switch {
	// If we add more signer algorithms to this utility in the future, we should make it an interface on the signer.
	case recoverRequest.Algorithm == algorithms.ECDSA_SECP256K1 && recoverRequest.PayloadType == signpayloads.OPAQUE_TO_RSV:
		var addr *ethtypes.Address0xHex
		signature, err := secp256k1.DecodeCompactRSV(ctx, recoverRequest.Signature)
		if err == nil {
			addr, err = signature.RecoverDirect(recoverRequest.Payload, d.dm.ethClientFactory.ChainID())
		}
		if err != nil {
			return nil, i18n.WrapError(ctx, err, msgs.MsgDomainABIRecoverRequestSignature)
		}
		return &prototk.RecoverSignerResponse{
			Verifier: addr.String(),
		}, nil
	default:
		return nil, i18n.NewError(ctx, msgs.MsgDomainABIRecoverRequestAlgorithm, recoverRequest.Algorithm)
	}
}

func (d *domain) InitDeploy(ctx context.Context, tx *components.PrivateContractDeploy) error {
	if tx.Inputs == nil {
		return i18n.NewError(ctx, msgs.MsgDomainTXIncompleteInitDeploy)
	}

	// Build the init request
	txSpec := &prototk.DeployTransactionSpecification{}
	tx.TransactionSpecification = txSpec
	txSpec.TransactionId = tktypes.Bytes32UUIDFirst16(tx.ID).String()
	txSpec.ConstructorParamsJson = tx.Inputs.String()

	// Do the request with the domain
	res, err := d.api.InitDeploy(ctx, &prototk.InitDeployRequest{
		Transaction: txSpec,
	})
	if err != nil {
		return err
	}

	// Store the response back on the TX
	tx.RequiredVerifiers = res.RequiredVerifiers
	return nil
}

func (d *domain) PrepareDeploy(ctx context.Context, tx *components.PrivateContractDeploy) error {
	if tx.Inputs == nil || tx.TransactionSpecification == nil || tx.Verifiers == nil {
		return i18n.NewError(ctx, msgs.MsgDomainTXIncompletePrepareDeploy)
	}

	// All the work is done for us by the engine in resolving the verifiers
	// after InitDeploy, so we just pass it along
	res, err := d.api.PrepareDeploy(ctx, &prototk.PrepareDeployRequest{
		Transaction:       tx.TransactionSpecification,
		ResolvedVerifiers: tx.Verifiers,
	})
	if err != nil {
		return err
	}

	if res.Signer != nil && *res.Signer != "" {
		tx.Signer = *res.Signer
	} else {
		switch d.config.BaseLedgerSubmitConfig.SubmitMode {
		case prototk.BaseLedgerSubmitConfig_ONE_TIME_USE_KEYS:
			tx.Signer = d.config.BaseLedgerSubmitConfig.OneTimeUsePrefix + tx.ID.String()
		default:
			log.L(ctx).Errorf("Signer mode %s and no signer returned", d.config.BaseLedgerSubmitConfig.SubmitMode)
			return i18n.NewError(ctx, msgs.MsgDomainDeployNoSigner)
		}
	}
	if res.Transaction != nil && res.Deploy == nil {
		var functionABI abi.Entry
		if err := json.Unmarshal(([]byte)(res.Transaction.FunctionAbiJson), &functionABI); err != nil {
			return i18n.WrapError(d.ctx, err, msgs.MsgDomainFactoryAbiJsonInvalid)
		}
		inputs, err := functionABI.Inputs.ParseJSONCtx(ctx, emptyJSONIfBlank(res.Transaction.ParamsJson))
		if err != nil {
			return err
		}
		tx.DeployTransaction = nil
		tx.InvokeTransaction = &components.EthTransaction{
			FunctionABI: &functionABI,
			To:          *d.RegistryAddress(),
			Inputs:      inputs,
		}
	} else if res.Deploy != nil && res.Transaction == nil {
		var functionABI abi.Entry
		if res.Deploy.ConstructorAbiJson == "" {
			// default constructor
			functionABI.Type = abi.Constructor
			functionABI.Inputs = abi.ParameterArray{}
		} else {
			if err := json.Unmarshal(([]byte)(res.Deploy.ConstructorAbiJson), &functionABI); err != nil {
				return i18n.WrapError(d.ctx, err, msgs.MsgDomainFactoryAbiJsonInvalid)
			}
		}
		inputs, err := functionABI.Inputs.ParseJSONCtx(ctx, emptyJSONIfBlank(res.Deploy.ParamsJson))
		if err != nil {
			return err
		}
		tx.DeployTransaction = &components.EthDeployTransaction{
			ConstructorABI: &functionABI,
			Bytecode:       res.Deploy.Bytecode,
			Inputs:         inputs,
		}
		tx.InvokeTransaction = nil
	} else {
		// Must specify exactly one of the two types of transaction
		return i18n.NewError(ctx, msgs.MsgDomainInvalidPrepareDeployResult)
	}
	return nil
}

func emptyJSONIfBlank(js string) []byte {
	if len(js) == 0 {
		return []byte(`{}`)
	}
	return []byte(js)
}

func (d *domain) close() {
	d.cancelCtx()
	<-d.initDone
}

func (d *domain) groupEventsByAddress(ctx context.Context, tx *gorm.DB, events []*blockindexer.EventWithData) (map[tktypes.EthAddress][]*blockindexer.EventWithData, map[tktypes.EthAddress]tktypes.HexBytes, error) {
	eventsByAddress := make(map[tktypes.EthAddress][]*blockindexer.EventWithData)
	configBytesByAddress := make(map[tktypes.EthAddress]tktypes.HexBytes)
	for _, ev := range events {
		// Note: hits will be cached, but events from unrecognized contracts will always
		// result in a cache miss and a database lookup
		// TODO: revisit if we should optimize this
		psc, err := d.dm.getSmartContractCached(ctx, tx, ev.Address)
		if err != nil {
			return nil, nil, err
		}
		if psc != nil && psc.Domain().Name() == d.name {
			eventsByAddress[ev.Address] = append(eventsByAddress[ev.Address], ev)
			configBytesByAddress[ev.Address] = psc.info.ConfigBytes
		}
	}
	return eventsByAddress, configBytesByAddress, nil
}

func (d *domain) handleEventBatch(ctx context.Context, tx *gorm.DB, batch *blockindexer.EventDeliveryBatch) (blockindexer.PostCommit, error) {
	// First index any domain contract deployments
	notifyTX, err := d.dm.registrationIndexer(ctx, tx, batch)
	if err != nil {
		return nil, err
	}

	// Then divide events by contract address and dispatch to the appropriate domain context
	transactionsComplete := make([]uuid.UUID, 0, len(batch.Events))
	eventsByAddress, configBytesByAddress, err := d.groupEventsByAddress(ctx, tx, batch.Events)
	if err != nil {
		return nil, err
	}
	for addr, events := range eventsByAddress {
		res, err := d.handleEventBatchForContract(ctx, batch.BatchID, addr, events, configBytesByAddress[addr])
		if err != nil {
			return nil, err
		}
		for _, txIDStr := range res.TransactionsComplete {
			txID, err := d.recoverTransactionID(ctx, txIDStr)
			if err != nil {
				return nil, err
			}
			transactionsComplete = append(transactionsComplete, *txID)
		}
	}

	return func() {
		notifyTX()
		for _, c := range transactionsComplete {
			inflight := d.dm.transactionWaiter.GetInflight(c)
			if inflight != nil {
				inflight.Complete(nil)
			}
		}
	}, nil
}

func (d *domain) recoverTransactionID(ctx context.Context, txIDString string) (*uuid.UUID, error) {
	txIDBytes, err := tktypes.ParseBytes32Ctx(ctx, txIDString)
	if err != nil {
		return nil, err
	}
	txUUID := txIDBytes.UUIDFirst16()
	return &txUUID, nil
}

func (d *domain) handleEventBatchForContract(ctx context.Context, batchID uuid.UUID, contractAddress tktypes.EthAddress, events []*blockindexer.EventWithData, configBytes tktypes.HexBytes) (*prototk.HandleEventBatchResponse, error) {
	var res *prototk.HandleEventBatchResponse
	eventsJSON, err := json.Marshal(events)
	if err == nil {
		res, err = d.api.HandleEventBatch(ctx, &prototk.HandleEventBatchRequest{
			BatchId:     batchID.String(),
			JsonEvents:  string(eventsJSON),
			ConfigBytes: configBytes.HexString(),
		})
	}
	if err != nil {
		return nil, err
	}

	spentStates := make(map[uuid.UUID][]string, len(res.SpentStates))
	for _, state := range res.SpentStates {
		txUUID, err := d.recoverTransactionID(ctx, state.TransactionId)
		if err != nil {
			return nil, err
		}
		spentStates[*txUUID] = append(spentStates[*txUUID], state.Id)
	}

	confirmedStates := make(map[uuid.UUID][]string, len(res.ConfirmedStates))
	for _, state := range res.ConfirmedStates {
		txUUID, err := d.recoverTransactionID(ctx, state.TransactionId)
		if err != nil {
			return nil, err
		}
		confirmedStates[*txUUID] = append(confirmedStates[*txUUID], state.Id)
	}

	newStates := make(map[uuid.UUID][]*statestore.StateUpsert, len(res.NewStates))
	for _, state := range res.NewStates {
		txUUID, err := d.recoverTransactionID(ctx, state.TransactionId)
		if err != nil {
			return nil, err
		}
		var id tktypes.HexBytes
		if state.Id != nil {
			id, err = tktypes.ParseHexBytes(ctx, *state.Id)
			if err != nil {
				return nil, err
			}
		}
		newStates[*txUUID] = append(newStates[*txUUID], &statestore.StateUpsert{
			ID:       id,
			SchemaID: state.SchemaId,
			Data:     tktypes.RawJSON(state.StateDataJson),
			Creating: true,
		})
	}

	err = d.dm.stateStore.RunInDomainContext(d.name, contractAddress, func(ctx context.Context, dsi statestore.DomainStateInterface) error {
		for txID, states := range newStates {
			if _, err = dsi.UpsertStates(&txID, states); err != nil {
				return err
			}
		}
		for txID, states := range spentStates {
			if err = dsi.MarkStatesSpent(txID, states); err != nil {
				return err
			}
		}
		for txID, states := range confirmedStates {
			if err = dsi.MarkStatesConfirmed(txID, states); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func (d *domain) getVerifier(ctx context.Context, algorithm string, verifierType string, privateKey []byte) (verifier string, err error) {
	res, err := d.api.GetVerifier(ctx, &prototk.GetVerifierRequest{
		Algorithm:    algorithm,
		VerifierType: verifierType,
		PrivateKey:   privateKey,
	})
	if err != nil {
		return "", err
	}
	return res.Verifier, nil
}

func (d *domain) sign(ctx context.Context, algorithm string, payloadType string, privateKey []byte, payload []byte) (signature []byte, err error) {
	res, err := d.api.Sign(ctx, &prototk.SignRequest{
		Algorithm:   algorithm,
		PayloadType: payloadType,
		PrivateKey:  privateKey,
		Payload:     payload,
	})
	if err != nil {
		return nil, err
	}
	return res.Payload, nil
}
