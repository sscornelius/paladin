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

package types

import (
	"context"

	"github.com/kaleido-io/paladin/kata/internal/statestore"
	"github.com/kaleido-io/paladin/kata/internal/transactionstore"
)

type StageProcessNextStep int

const (
	NextStepWait StageProcessNextStep = iota
	NextStepNewStage
	NextStepNewAction
)

type StageEvent struct {
	ID    string      `json:"id"` // TODO: not sure how useful it is to have this ID as the process of event should be idempotent?
	Stage string      `json:"stage"`
	TxID  string      `json:"transactionId"`
	Data  interface{} `json:"data"` // schema decided by each stage
}

type TxProcessPreReq struct {
	TxIDs []string `json:"transactionIds,omitempty"`
}

// defines the methods for checking whether a transaction's dependents matches a specific criteria
type DependencyChecker interface {
	PreReqsMatchCondition(ctx context.Context, preReqTxIDs []string, conditionFunc func(tsg transactionstore.TxStateGetters) (preReqComplete bool)) (filteredPreReqTxIDs []string)
	GetPreReqDispatchAddresses(ctx context.Context, preReqTxIDs []string) (dispatchAddresses []string)
	RegisterPreReqTrigger(ctx context.Context, txID string, txPreReq *TxProcessPreReq)
}

type MockIdentityResolver struct {
}

func (mti *MockIdentityResolver) IsCurrentNode(nodeID string) bool {
	return nodeID == "current-node"
}

func (mti *MockIdentityResolver) GetDispatchAddress(preferredAddresses []string) string {
	if len(preferredAddresses) > 0 {
		return preferredAddresses[0]
	}
	return ""
}

func (mti *MockIdentityResolver) ConnectToBaseLeger() error {
	return nil
}

type IdentityResolver interface {
	IsCurrentNode(nodeID string) bool
	ConnectToBaseLeger() error // TODO: does this function connects to the base ledger of current node/any available node as well? How about events?
	GetDispatchAddress(preferredAddresses []string) string
}

type StageFoundationService interface {
	IdentityResolver() IdentityResolver
	DependencyChecker() DependencyChecker
	StateStore() statestore.StateStore // TODO: filter out to only getters so setters can be coordinated efficiently like transactions
}

type PaladinStageFoundationService struct {
	dependencyChecker   DependencyChecker
	stateStore          statestore.StateStore
	nodeAndWalletLookUp IdentityResolver
}

func (psfs *PaladinStageFoundationService) DependencyChecker() DependencyChecker {
	return psfs.dependencyChecker
}

func (psfs *PaladinStageFoundationService) StateStore() statestore.StateStore {
	return psfs.stateStore
}

func (psfs *PaladinStageFoundationService) IdentityResolver() IdentityResolver {
	return psfs.nodeAndWalletLookUp
}

func NewPaladinStageFoundationService(dependencyChecker DependencyChecker,
	stateStore statestore.StateStore,
	nodeAndWalletLookUp IdentityResolver) StageFoundationService {
	return &PaladinStageFoundationService{
		dependencyChecker:   dependencyChecker,
		stateStore:          stateStore,
		nodeAndWalletLookUp: nodeAndWalletLookUp,
	}
}
