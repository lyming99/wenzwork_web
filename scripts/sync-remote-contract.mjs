import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const arguments_ = process.argv.slice(2);
const check = arguments_.includes("--check");
const fixtureArgument = arguments_.indexOf("--flutter-fixture");
const flutterFixture =
  fixtureArgument >= 0
    ? resolve(root, arguments_[fixtureArgument + 1])
    : resolve(root, "../wenzwork/test/fixtures/remote_rpc_v2_contract.json");
const artifacts = [
  {
    source: resolve(root, "api/remote/v1/fixtures/rpc_v2_contract.json"),
    targets: [
      resolve(root, "web/src/remote/fixtures/rpc_v2_contract.json"),
      flutterFixture,
    ],
  },
  {
    source: resolve(
      root,
      "api/remote/v1/fixtures/protocol_golden_vectors.json",
    ),
    targets: [
      resolve(root, "web/src/remote/fixtures/protocol_golden_vectors.json"),
      resolve(dirname(flutterFixture), "protocol_golden_vectors.json"),
    ],
  },
];

const normalize = (value) => {
  if (Array.isArray(value)) return value.map(normalize);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value)
        .sort()
        .map((key) => [key, normalize(value[key])]),
    );
  }
  return value;
};
const semanticText = (value) =>
  `${JSON.stringify(normalize(value), null, 2)}\n`;
const sourceCommit = execFileSync("git", ["rev-parse", "HEAD"], {
  cwd: root,
  encoding: "utf8",
}).trim();
const hashes = [];

for (const artifact of artifacts) {
  const source = JSON.parse(readFileSync(artifact.source, "utf8"));
  if (
    !Number.isSafeInteger(source.contractVersion) ||
    source.contractVersion < 1
  ) {
    throw new Error(
      `The canonical artifact has no valid contractVersion: ${artifact.source}`,
    );
  }
  const normalized = semanticText(source);
  const sha256 = createHash("sha256").update(normalized).digest("hex");
  hashes.push(sha256);
  for (const target of artifact.targets) {
    const metadataPath = `${target}.meta.json`;
    if (check) {
      if (
        !existsSync(target) ||
        semanticText(JSON.parse(readFileSync(target, "utf8"))) !== normalized
      ) {
        throw new Error(`Remote contract drift detected: ${target}`);
      }
      if (!existsSync(metadataPath)) {
        throw new Error(`Missing remote contract metadata: ${metadataPath}`);
      }
      const metadata = JSON.parse(readFileSync(metadataPath, "utf8"));
      if (
        metadata.contractVersion !== source.contractVersion ||
        metadata.sha256 !== sha256 ||
        typeof metadata.generatedAt !== "string" ||
        !/^\d{4}-\d{2}-\d{2}T/.test(metadata.generatedAt) ||
        typeof metadata.sourceCommit !== "string" ||
        !/^[0-9a-f]{40}$/.test(metadata.sourceCommit)
      ) {
        throw new Error(`Invalid remote contract metadata: ${metadataPath}`);
      }
      continue;
    }
    writeFileSync(target, normalized);
    writeFileSync(
      metadataPath,
      `${JSON.stringify(
        {
          contractVersion: source.contractVersion,
          sourceCommit,
          sha256,
          generatedAt: new Date().toISOString(),
        },
        null,
        2,
      )}\n`,
    );
  }
}

console.log(
  `${check ? "checked" : "synchronized"} remote artifacts ${hashes.join(" ")}`,
);
