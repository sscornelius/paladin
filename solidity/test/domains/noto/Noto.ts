import { loadFixture } from "@nomicfoundation/hardhat-toolbox/network-helpers";
import { expect } from "chai";
import { randomBytes } from "crypto";
import {
  AbiCoder,
  ContractTransactionReceipt,
  Signer,
  TypedDataEncoder,
} from "ethers";
import hre, { ethers } from "hardhat";
import { NotoFactory, Noto } from "../../../typechain-types";

export async function newTransferHash(
  noto: Noto,
  inputs: string[],
  outputs: string[],
  data: string
) {
  const domain = {
    name: "noto",
    version: "0.0.1",
    chainId: hre.network.config.chainId,
    verifyingContract: await noto.getAddress(),
  };
  const types = {
    Transfer: [
      { name: "inputs", type: "bytes32[]" },
      { name: "outputs", type: "bytes32[]" },
      { name: "data", type: "bytes" },
    ],
  };
  const value = { inputs, outputs, data };
  return {
    hash: TypedDataEncoder.hash(domain, types, value),
  };
}

export function randomBytes32() {
  return "0x" + Buffer.from(randomBytes(32)).toString("hex");
}

export function fakeTXO() {
  return randomBytes32();
}

export async function deployNotoInstance(
  notoFactory: NotoFactory,
  notary: string
) {
  const abi = AbiCoder.defaultAbiCoder();
  const deployTx = await notoFactory.deploy(randomBytes32(), notary, "0x");
  const deployReceipt = await deployTx.wait();
  const deployEvent = deployReceipt?.logs.find(
    (l) =>
      notoFactory.interface.parseLog(l)?.name ===
      "PaladinRegisterSmartContract_V0"
  );
  expect(deployEvent).to.exist;
  return deployEvent && "args" in deployEvent ? deployEvent.args.instance : "";
}

describe("Noto", function () {
  async function deployNotoFixture() {
    const [notary, other] = await ethers.getSigners();

    const NotoFactory = await ethers.getContractFactory("NotoFactory");
    const notoFactory = await NotoFactory.deploy();
    const Noto = await ethers.getContractFactory("Noto");
    const noto = Noto.attach(
      await deployNotoInstance(notoFactory, notary.address)
    );

    return { noto: noto as Noto, notary, other };
  }

  async function doTransfer(
    notary: Signer,
    noto: Noto,
    inputs: string[],
    outputs: string[],
    data: string
  ) {
    const tx = await noto.connect(notary).transfer(inputs, outputs, "0x", data);
    const results: ContractTransactionReceipt | null = await tx.wait();

    for (const log of results?.logs || []) {
      const event = noto.interface.parseLog(log as any);
      expect(event?.args.inputs).to.deep.equal(inputs);
      expect(event?.args.outputs).to.deep.equal(outputs);
      expect(event?.args.data).to.deep.equal(data);
    }
    for (const input of inputs) {
      expect(await noto.isUnspent(input)).to.equal(false);
    }
    for (const output of outputs) {
      expect(await noto.isUnspent(output)).to.equal(true);
    }
  }

  it("UTXO lifecycle and double-spend protections", async function () {
    const { noto, notary } = await loadFixture(deployNotoFixture);

    const txo1 = fakeTXO();
    const txo2 = fakeTXO();
    const txo3 = fakeTXO();

    // Make two UTXOs
    await doTransfer(notary, noto, [], [txo1, txo2], randomBytes32());

    // Check for double-mint protection
    await expect(
      doTransfer(notary, noto, [], [txo1], randomBytes32())
    ).rejectedWith("NotoInvalidOutput");

    // Check for spend unknown protection
    await expect(
      doTransfer(notary, noto, [txo3], [], randomBytes32())
    ).rejectedWith("NotoInvalidInput");

    // Spend one
    await doTransfer(notary, noto, [txo1], [txo3], randomBytes32());

    // Check for double-spend protection
    await expect(
      doTransfer(notary, noto, [txo1], [txo3], randomBytes32())
    ).rejectedWith("NotoInvalidInput");

    // Spend another
    await doTransfer(notary, noto, [txo2], [], randomBytes32());

    // Spend the last one
    await doTransfer(notary, noto, [txo3], [], randomBytes32());
  });
});
