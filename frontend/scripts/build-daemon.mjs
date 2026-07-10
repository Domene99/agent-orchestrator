import { rmSync, mkdirSync, existsSync, copyFileSync, chmodSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { createWriteStream } from "node:fs";
import { pipeline } from "node:stream/promises";

const scriptsDir = dirname(fileURLToPath(import.meta.url));
const frontendRoot = resolve(scriptsDir, "..");
const repoRoot = resolve(frontendRoot, "..");
const backendRoot = join(repoRoot, "backend");
const outDir = join(frontendRoot, "daemon");
const outPath = join(outDir, process.platform === "win32" ? "ao.exe" : "ao");

const DRUN_GITHUB_REPO = "dmosc/drun";
const DRUN_VERSION = process.env.DRUN_VERSION ?? "latest";

rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

// Map Node.js platform/arch to the drun-mcp release asset name.
function drunAssetName() {
	const { platform, arch } = process;
	if (platform === "darwin" && arch === "arm64") return "drun-mcp-macos-arm64";
	if (platform === "linux" && arch === "x64") return "drun-mcp-linux-x86_64";
	return null; // unsupported platform — build will proceed without drun
}

async function downloadDrunMCP(destPath) {
	const asset = drunAssetName();
	if (!asset) {
		console.warn(`No drun-mcp release binary for ${process.platform}/${process.arch}; building ao without drun.`);
		return false;
	}

	// Resolve "latest" to a concrete tag via the GitHub API.
	let tag = DRUN_VERSION;
	if (tag === "latest") {
		const apiUrl = `https://api.github.com/repos/${DRUN_GITHUB_REPO}/releases/latest`;
		const res = await fetch(apiUrl, { headers: { "User-Agent": "ao-build" } });
		if (!res.ok) {
			console.warn(`Could not resolve latest drun release (${res.status}); building ao without drun.`);
			return false;
		}
		const json = await res.json();
		tag = json.tag_name;
	}

	const url = `https://github.com/${DRUN_GITHUB_REPO}/releases/download/${tag}/${asset}`;
	console.log(`Downloading drun-mcp ${tag} from GitHub…`);

	const res = await fetch(url, { headers: { "User-Agent": "ao-build" } });
	if (!res.ok) {
		console.warn(`Download failed (${res.status} ${url}); building ao without drun.`);
		return false;
	}

	const embedDir = join(backendRoot, "internal", "drun", "binaries");
	mkdirSync(embedDir, { recursive: true });
	const writer = createWriteStream(destPath);
	await pipeline(res.body, writer);
	chmodSync(destPath, 0o755);
	console.log(`drun-mcp ${tag} ready.`);
	return true;
}

const drunEmbedTarget = join(backendRoot, "internal", "drun", "binaries", "drun-mcp");
let buildTags = [];

const bundled = await downloadDrunMCP(drunEmbedTarget);
if (bundled) {
	buildTags = ["-tags", "bundled_drun"];
}

const result = spawnSync("go", ["build", ...buildTags, "-o", outPath, "./cmd/ao"], {
	cwd: backendRoot,
	stdio: "inherit",
});

if (result.error) {
	console.error(`failed to start go build: ${result.error.message}`);
	process.exit(1);
}

if (result.status !== 0) {
	process.exit(result.status ?? 1);
}
