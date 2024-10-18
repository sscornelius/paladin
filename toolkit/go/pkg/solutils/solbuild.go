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

package solutils

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-signer/pkg/abi"
	"github.com/kaleido-io/paladin/toolkit/pkg/tkmsgs"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"
)

type SolidityBuild struct {
	ABI      abi.ABI          `json:"abi"`
	Bytecode tktypes.HexBytes `json:"bytecode"`
}

type SolidityBuildWithLinks struct {
	ABI            abi.ABI                                       `json:"abi"`
	Bytecode       string                                        `json:"bytecode"`
	LinkReferences map[string]map[string][]SolidityLinkReference `json:"linkReferences"`
}

type SolidityLinkReference struct {
	Start  int `json:"start"`
	Length int `json:"length"`
}

func MustLoadBuild(buildOutput []byte) *SolidityBuild {
	return MustLoadBuildResolveLinks(buildOutput, map[string]*tktypes.EthAddress{})
}

func MustLoadBuildResolveLinks(buildOutput []byte, libraries map[string]*tktypes.EthAddress) *SolidityBuild {
	build, err := LoadBuildResolveLinks(context.Background(), buildOutput, libraries)
	if err != nil {
		panic(err)
	}
	return build
}

func LoadBuild(ctx context.Context, buildOutput []byte) (build *SolidityBuild, err error) {
	return LoadBuildResolveLinks(ctx, buildOutput, map[string]*tktypes.EthAddress{})
}

func LoadBuildResolveLinks(ctx context.Context, buildOutput []byte, libraries map[string]*tktypes.EthAddress) (build *SolidityBuild, err error) {
	var unresolved SolidityBuildWithLinks
	err = json.Unmarshal(buildOutput, &unresolved)
	if err == nil {
		build = &SolidityBuild{ABI: unresolved.ABI}
		build.Bytecode, err = linkBytecode(ctx, unresolved, libraries)
	}
	if err != nil {
		return nil, err
	}
	return build, nil
}

// linkBytecode: performs linking by replacing placeholders with deployed addresses
// See https://docs.soliditylang.org/en/latest/using-the-compiler.html#library-linking
func linkBytecode(ctx context.Context, artifact SolidityBuildWithLinks, libraries map[string]*tktypes.EthAddress) (tktypes.HexBytes, error) {
	bytecode := artifact.Bytecode
	for fileName, fileReferences := range artifact.LinkReferences {
		for libName, link := range fileReferences {
			fullLibName := fmt.Sprintf("%s:%s", fileName, libName)
			addr, found := libraries[fullLibName]
			if !found {
				addr, found = libraries[libName]
			}
			if !found {
				return nil, i18n.NewError(ctx, tkmsgs.MsgSolBuildMissingLink, fullLibName)
			}
			for _, link := range link {
				start := 2 /* 0x */ + link.Start*2
				// Format from 0.5.0 onwards is __$53aea86b7d70b31448b230b20ae141a537$__
				// Where "53aea86b7d70b31448b230b20ae141a537" above is a 34 character prefix of the hex
				// encoding of the keccak256 hash of the fully qualified library name
				end := start + 3 /* __$ */ + 34 /* placeholder */ + 3 /* $__ */
				placeholder := string(bytecode[start+3 : start+34+3])
				expectedPlaceholder := tktypes.Bytes32Keccak([]byte(fullLibName)).HexString()[0:34]
				if placeholder != expectedPlaceholder {
					return nil, i18n.NewError(ctx, tkmsgs.MsgSolBuildParseFailed, start, fullLibName, placeholder, expectedPlaceholder)
				}
				bytecode = bytecode[0:start] + addr.String()[2:] /* no 0x prefix */ + bytecode[end:]
			}
		}
	}
	return hex.DecodeString(strings.TrimPrefix(bytecode, "0x"))
}
