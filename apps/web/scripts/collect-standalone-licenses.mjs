#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  copyFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join, relative, resolve, sep } from "node:path";

const LEGAL_FILE = /^(?:licen[cs]e|copying|notice|copyright|patents|authors|contributors)(?:[._-].*)?$/i;
const REJECTED_LICENSE = /^(?:unlicen[cs]ed|unknown|none)$/i;
const NATIVE_IMAGE_PACKAGE = /^(?:sharp|@img\/)/;

function parseArguments(argv) {
  const options = {
    output: ".next/licenses",
    projectLicense: "../../LICENSE",
    sourceRoot: ".",
    standalone: ".next/standalone",
  };

  for (let index = 0; index < argv.length; index += 2) {
    const flag = argv[index];
    const value = argv[index + 1];
    if (!flag?.startsWith("--") || value === undefined) {
      throw new Error(`expected --name value arguments, received ${argv.join(" ")}`);
    }
    const key = {
      "--output": "output",
      "--project-license": "projectLicense",
      "--source-root": "sourceRoot",
      "--standalone": "standalone",
    }[flag];
    if (!key) {
      throw new Error(`unknown argument ${flag}`);
    }
    options[key] = value;
  }

  return Object.fromEntries(
    Object.entries(options).map(([key, value]) => [key, resolve(value)]),
  );
}

function walkFiles(root) {
  const files = [];
  const visit = (directory) => {
    for (const entry of readdirSync(directory, { withFileTypes: true }).sort((left, right) =>
      left.name.localeCompare(right.name, "en"),
    )) {
      const entryPath = join(directory, entry.name);
      if (entry.isDirectory()) {
        visit(entryPath);
      } else if (entry.isFile()) {
        files.push(entryPath);
      }
    }
  };
  visit(root);
  return files;
}

function readPackage(packagePath) {
  try {
    return JSON.parse(readFileSync(packagePath, "utf8"));
  } catch (error) {
    throw new Error(`cannot parse ${packagePath}: ${error.message}`);
  }
}

function isInside(root, candidate) {
  const pathFromRoot = relative(root, candidate);
  return pathFromRoot === "" || (!pathFromRoot.startsWith(`..${sep}`) && pathFromRoot !== "..");
}

function legalFiles(directory) {
  return readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && LEGAL_FILE.test(entry.name))
    .map((entry) => join(directory, entry.name))
    .sort((left, right) => basename(left).localeCompare(basename(right), "en"));
}

function declaredLicense(metadata) {
  if (typeof metadata.license === "string") {
    const value = metadata.license.trim();
    if (value !== "" && !REJECTED_LICENSE.test(value) && !/^SEE LICEN[CS]E IN\s+/i.test(value)) {
      return value;
    }
  }
  if (Array.isArray(metadata.licenses)) {
    const values = metadata.licenses
      .map((license) => (typeof license === "string" ? license : license?.type))
      .filter((license) => typeof license === "string" && license.trim() !== "")
      .map((license) => license.trim());
    if (values.length > 0 && values.every((license) => !REJECTED_LICENSE.test(license))) {
      return values.join(" OR ");
    }
  }
  return null;
}

function seeLicenseFile(directory, metadata) {
  if (typeof metadata.license !== "string") {
    return [];
  }
  const match = /^SEE LICEN[CS]E IN\s+(.+)$/i.exec(metadata.license.trim());
  if (!match) {
    return [];
  }
  const target = resolve(directory, match[1]);
  if (!isInside(directory, target) || !existsSync(target) || !lstatSync(target).isFile()) {
    throw new Error(`${join(directory, "package.json")} refers to missing license file ${match[1]}`);
  }
  return [target];
}

function findEvidence(packageDirectory, metadata, sourceRoot) {
  const directFiles = [...new Set([...legalFiles(packageDirectory), ...seeLicenseFile(packageDirectory, metadata)])];
  if (directFiles.length > 0) {
    return { declaration: declaredLicense(metadata), files: directFiles, inheritedFrom: null };
  }

  const declaration = declaredLicense(metadata);
  if (declaration) {
    return { declaration, files: [], inheritedFrom: null };
  }

  // Next ships a few internal dist/compiled package manifests without their
  // own declaration. Walk only within the containing installed package and
  // use its exact license evidence; never fall back to an unrelated project.
  let ancestor = dirname(packageDirectory);
  while (isInside(sourceRoot, ancestor) && ancestor !== sourceRoot) {
    const ancestorManifest = join(ancestor, "package.json");
    if (existsSync(ancestorManifest) && lstatSync(ancestorManifest).isFile()) {
      const ancestorMetadata = readPackage(ancestorManifest);
      const ancestorFiles = [
        ...new Set([...legalFiles(ancestor), ...seeLicenseFile(ancestor, ancestorMetadata)]),
      ];
      const ancestorDeclaration = declaredLicense(ancestorMetadata);
      if (ancestorFiles.length > 0 || ancestorDeclaration) {
        return {
          declaration: ancestorDeclaration,
          files: ancestorFiles,
          inheritedFrom: relative(sourceRoot, ancestorManifest).split(sep).join("/"),
        };
      }
    }
    ancestor = dirname(ancestor);
  }

  throw new Error(
    `no license evidence for ${metadata.name}@${metadata.version ?? "unknown"} (${packageDirectory})`,
  );
}

function evidenceDestination(output, source) {
  const contents = readFileSync(source);
  const digest = createHash("sha256").update(contents).digest("hex");
  const safeName = basename(source).replace(/[^A-Za-z0-9._-]/g, "_");
  const destination = join(output, "node", "evidence", `${digest}-${safeName}`);
  mkdirSync(dirname(destination), { recursive: true });
  if (!existsSync(destination)) {
    writeFileSync(destination, contents, { mode: 0o644 });
  }
  return {
    path: relative(output, destination).split(sep).join("/"),
    sha256: digest,
  };
}

function main() {
  const options = parseArguments(process.argv.slice(2));
  for (const [name, target] of Object.entries({
    projectLicense: options.projectLicense,
    sourceRoot: options.sourceRoot,
    standalone: options.standalone,
  })) {
    if (!existsSync(target)) {
      throw new Error(`${name} does not exist: ${target}`);
    }
  }

  const files = walkFiles(options.standalone);
  const forbidden = files
    .map((file) => relative(options.standalone, file).split(sep).join("/"))
    .filter((file) => file.split("/").some((component) => /^(?:sharp|@img|.*libvips.*)$/i.test(component)));
  if (forbidden.length > 0) {
    throw new Error(`native image packages remain in standalone output:\n${forbidden.join("\n")}`);
  }

  const packageManifests = files
    .filter((file) => basename(file) === "package.json")
    .map((standaloneManifest) => ({
      metadata: readPackage(standaloneManifest),
      relativeManifest: relative(options.standalone, standaloneManifest),
      standaloneManifest,
    }))
    .filter(
      ({ metadata, relativeManifest }) =>
        relativeManifest.split(sep).includes("node_modules") &&
        typeof metadata.name === "string" &&
        metadata.name.trim() !== "",
    )
    .sort((left, right) => left.relativeManifest.localeCompare(right.relativeManifest, "en"));

  rmSync(options.output, { force: true, recursive: true });
  mkdirSync(join(options.output, "project"), { recursive: true });
  copyFileSync(options.projectLicense, join(options.output, "LICENSE"));
  const projectMetadata = join(options.standalone, "package.json");
  if (!existsSync(projectMetadata)) {
    throw new Error(`standalone project metadata does not exist: ${projectMetadata}`);
  }
  copyFileSync(projectMetadata, join(options.output, "project", "package.json"));

  const packages = packageManifests.map((entry, index) => {
    const sourceManifest = join(options.sourceRoot, entry.relativeManifest);
    if (!isInside(options.sourceRoot, sourceManifest) || !existsSync(sourceManifest)) {
      throw new Error(`source package metadata does not exist: ${sourceManifest}`);
    }
    const sourceMetadata = readPackage(sourceManifest);
    if (sourceMetadata.name !== entry.metadata.name || sourceMetadata.version !== entry.metadata.version) {
      throw new Error(`standalone metadata does not match source metadata: ${entry.relativeManifest}`);
    }
    if (NATIVE_IMAGE_PACKAGE.test(sourceMetadata.name)) {
      throw new Error(`forbidden native image package ${sourceMetadata.name} remains in standalone output`);
    }

    const packageNumber = String(index + 1).padStart(4, "0");
    const metadataDestination = join(options.output, "node", "packages", packageNumber, "package.json");
    mkdirSync(dirname(metadataDestination), { recursive: true });
    copyFileSync(sourceManifest, metadataDestination);

    const evidence = findEvidence(dirname(sourceManifest), sourceMetadata, options.sourceRoot);
    return {
      name: sourceMetadata.name,
      version: sourceMetadata.version ?? null,
      standalonePath: entry.relativeManifest.split(sep).join("/"),
      packageMetadata: relative(options.output, metadataDestination).split(sep).join("/"),
      licenseDeclaration: evidence.declaration,
      inheritedFrom: evidence.inheritedFrom,
      evidence: evidence.files.map((file) => ({
        source: relative(options.sourceRoot, file).split(sep).join("/"),
        ...evidenceDestination(options.output, file),
      })),
    };
  });

  const manifest = {
    schemaVersion: 1,
    projectLicense: "LICENSE",
    projectMetadata: "project/package.json",
    packages,
  };
  writeFileSync(join(options.output, "THIRD_PARTY.json"), `${JSON.stringify(manifest, null, 2)}\n`, {
    mode: 0o644,
  });
  process.stdout.write(`collected license evidence for ${packages.length} standalone packages\n`);
}

try {
  main();
} catch (error) {
  process.stderr.write(`${error.stack ?? error.message}\n`);
  process.exitCode = 1;
}
