"use client";

import { useRef, useState, type ChangeEvent } from "react";

import { DismissGuardNote } from "@/components/app/dismiss-guard-note";
import { usePlatform } from "@/components/app/platform-provider";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type { CreateDeployResponse, Site } from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getHostingClient } from "@/lib/platform-client";
import { operatorFetch } from "@/lib/operator-transport";
import { formatBytes } from "@/lib/platform-model";

export type DeployDialogProps = {
  zone: Zone;
  site: Site;
  /** `HOSTING_UPLOADS_ENABLED`: hides the Upload folder tab when false. */
  uploadsEnabled: boolean;
  /** `HOSTING_UPLOAD_MAX_BYTES`, for the client-side size check. */
  uploadMaxBytes: number;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: CreateDeployResponse): void;
};

const repositoryPattern =
  /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(\.git)?$/;

const maxUploadFiles = 5_000;

/**
 * The same ignore rules the hosting engine applies, so the file and byte counts on
 * screen are the ones that are actually sent. The server enforces them again.
 */
function isSkipped(relativePath: string): boolean {
  const parts = relativePath.split("/");
  if (parts.some((part) => part === ".git")) {
    return true;
  }
  const name = parts[parts.length - 1] ?? "";
  if (name === ".npmrc" || name.startsWith("credentials")) {
    return true;
  }
  if (name === ".env" || name.startsWith(".env.")) {
    return true;
  }
  return /\.(pem|key|p12|pfx)$/i.test(name);
}

const relativePathOf = (file: File) => file.webkitRelativePath || file.name;

/**
 * `note` is the control plane's own account of what the upload left out — a
 * manifest.json of the folder's own would shadow the one the build adds, so it
 * is not staged. The deploy still starts; the dialog stays open to say so
 * rather than dropping the sentence on the way to the deploy list.
 */
type UploadResponse = { upload_id?: string; note?: string };

/**
 * Starts one deploy: from a GitHub repository, or from a folder picked in the browser
 * when the control plane enables uploads.
 *
 * The per-deploy token is never prefilled, never stored and never sent anywhere but the
 * control plane's `CreateDeploy` call; the field is cleared as soon as that promise
 * settles, and closing the dialog unmounts the component, which drops it entirely.
 */
export function DeployDialog({
  zone,
  site,
  uploadsEnabled,
  uploadMaxBytes,
  open,
  onOpenChange,
  onDone,
}: DeployDialogProps) {
  const { invalidate } = usePlatform();
  const [tab, setTab] = useState("github");
  const [repository, setRepository] = useState(site.repository);
  const [revision, setRevision] = useState(site.branch || "main");
  const [path, setPath] = useState(site.path);
  const [showToken, setShowToken] = useState(false);
  const [token, setToken] = useState("");
  const [files, setFiles] = useState<File[]>([]);
  const [folderName, setFolderName] = useState("");
  const [busy, setBusy] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string>();
  const [droppedNote, setDroppedNote] = useState<string>();
  const abortRef = useRef<AbortController | undefined>(undefined);

  const trimmedRepository = repository.trim();
  const repositoryInvalid =
    trimmedRepository.length > 0 && !repositoryPattern.test(trimmedRepository);
  const totalBytes = files.reduce((sum, file) => sum + file.size, 0);
  const tooLarge = uploadMaxBytes > 0 && totalBytes > uploadMaxBytes;
  const tooManyFiles = files.length > maxUploadFiles;
  const wasPrivate = site.latest?.source?.privateRepository ?? false;

  function pickFiles(event: ChangeEvent<HTMLInputElement>) {
    const picked = Array.from(event.target.files ?? []);
    const kept = picked.filter((file) => !isSkipped(relativePathOf(file)));
    const first = kept[0] ?? picked[0];
    const firstPath = first ? relativePathOf(first) : "";
    setFolderName(firstPath.includes("/") ? firstPath.split("/")[0] : "");
    setFiles(kept);
    setError(undefined);
  }

  function close() {
    abortRef.current?.abort();
    setToken("");
    onOpenChange(false);
  }

  async function deployFromGit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getHostingClient().createDeploy({
        zoneId: zone.id,
        repository: trimmedRepository,
        revision: revision.trim(),
        path: path.trim(),
        gitToken: token,
      });
      await invalidate();
      onDone?.(result);
      onOpenChange(false);
    } catch (caught) {
      setError(describeError(caught));
      setBusy(false);
    } finally {
      // The token is good for exactly one build; it never survives the request.
      setToken("");
    }
  }

  async function deployFromFolder() {
    setError(undefined);
    setBusy(true);
    setUploading(true);
    const abort = new AbortController();
    abortRef.current = abort;
    try {
      const body = new FormData();
      body.append("zone_id", zone.id);
      for (const file of files) {
        body.append("files", file, relativePathOf(file));
      }
      const response = await operatorFetch("/hosting/v1/uploads", {
        method: "POST",
        body,
        signal: abort.signal,
      });
      const payload = (await response.json()) as UploadResponse;
      const uploadId = payload.upload_id ?? "";
      if (!uploadId) {
        throw new Error("The upload did not return an id.");
      }
      const note = payload.note ?? "";
      setUploading(false);
      const result = await getHostingClient().createDeploy({
        zoneId: zone.id,
        uploadId,
      });
      await invalidate();
      onDone?.(result);
      if (note) {
        // The build is running either way; closing here would take the only
        // report of the files it is not building with off the screen.
        setDroppedNote(note);
        setBusy(false);
        return;
      }
      onOpenChange(false);
    } catch (caught) {
      if (!abort.signal.aborted) {
        setError(describeError(caught));
      }
      setUploading(false);
      setBusy(false);
    }
  }

  if (!open) {
    return null;
  }

  // The upload left files out and the deploy is already running: the only
  // honest thing left to do is say which, before the dialog goes away.
  if (droppedNote) {
    return (
      <Dialog
        open
        onOpenChange={(next) => {
          if (!next) {
            onOpenChange(false);
          }
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Deploying {zone.name}</DialogTitle>
            <DialogDescription>
              The build started with the files the upload kept.
            </DialogDescription>
          </DialogHeader>
          <p role="status" className="text-ui text-subtle">
            {droppedNote}
          </p>
          <DialogFooter>
            <Button onClick={() => onOpenChange(false)}>Close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  const gitDisabled =
    busy || repositoryInvalid || trimmedRepository.length === 0;
  const uploadDisabled = busy || files.length === 0 || tooLarge || tooManyFiles;
  // An upload can be abandoned (the AbortController cancels the request), so Cancel
  // and Escape both stay live for it. `CreateDeploy` cannot: once the request is out
  // there is nothing to abort, and Cancel is disabled. Escape has to agree with the
  // button rather than quietly do what the button refuses to.
  const locked = busy && !uploading;

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next) {
          close();
          return;
        }
        onOpenChange(next);
      }}
    >
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg"
        showCloseButton={!locked}
        onEscapeKeyDown={(event) => {
          if (locked) {
            event.preventDefault();
          }
        }}
        onInteractOutside={(event) => {
          if (locked) {
            event.preventDefault();
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>Deploy {zone.name}</DialogTitle>
          <DialogDescription>
            The hosting engine builds the source and puts the result behind{" "}
            {zone.name}.
          </DialogDescription>
        </DialogHeader>

        {uploadsEnabled ? (
          <Tabs value={tab} onValueChange={setTab}>
            <TabsList>
              <TabsTrigger value="github" disabled={busy}>
                GitHub
              </TabsTrigger>
              <TabsTrigger value="upload" disabled={busy}>
                Upload folder
              </TabsTrigger>
            </TabsList>
            <TabsContent value="github">
              <GitFields
                busy={busy}
                repository={repository}
                onRepository={setRepository}
                repositoryInvalid={repositoryInvalid}
                revision={revision}
                onRevision={setRevision}
                path={path}
                onPath={setPath}
                showToken={showToken}
                onShowToken={setShowToken}
                token={token}
                onToken={setToken}
                wasPrivate={wasPrivate}
              />
            </TabsContent>
            <TabsContent value="upload">
              <UploadFields
                busy={busy}
                uploading={uploading}
                files={files.length}
                folderName={folderName}
                totalBytes={totalBytes}
                uploadMaxBytes={uploadMaxBytes}
                tooLarge={tooLarge}
                tooManyFiles={tooManyFiles}
                onPick={pickFiles}
              />
            </TabsContent>
          </Tabs>
        ) : (
          <GitFields
            busy={busy}
            repository={repository}
            onRepository={setRepository}
            repositoryInvalid={repositoryInvalid}
            revision={revision}
            onRevision={setRevision}
            path={path}
            onPath={setPath}
            showToken={showToken}
            onShowToken={setShowToken}
            token={token}
            onToken={setToken}
            wasPrivate={wasPrivate}
          />
        )}

        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}

        <DismissGuardNote active={locked} action="Starting the deploy" />

        <DialogFooter>
          <Button variant="outline" onClick={close} disabled={locked}>
            Cancel
          </Button>
          <Button
            onClick={() => {
              if (uploadsEnabled && tab === "upload") {
                void deployFromFolder();
              } else {
                void deployFromGit();
              }
            }}
            disabled={
              uploadsEnabled && tab === "upload" ? uploadDisabled : gitDisabled
            }
          >
            {busy ? (uploading ? "Uploading…" : "Starting…") : "Deploy"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function GitFields({
  busy,
  repository,
  onRepository,
  repositoryInvalid,
  revision,
  onRevision,
  path,
  onPath,
  showToken,
  onShowToken,
  token,
  onToken,
  wasPrivate,
}: {
  busy: boolean;
  repository: string;
  onRepository(value: string): void;
  repositoryInvalid: boolean;
  revision: string;
  onRevision(value: string): void;
  path: string;
  onPath(value: string): void;
  showToken: boolean;
  onShowToken(value: boolean): void;
  token: string;
  onToken(value: string): void;
  wasPrivate: boolean;
}) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="deploy-repository">Repository</Label>
        <Input
          id="deploy-repository"
          value={repository}
          onChange={(event) => onRepository(event.target.value)}
          placeholder="https://github.com/owner/name"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          disabled={busy}
          aria-invalid={repositoryInvalid || undefined}
          className="font-mono text-[13px]"
        />
        {repositoryInvalid ? (
          <p role="alert" className="text-xs text-destructive">
            Use the https://github.com/owner/name URL.
          </p>
        ) : null}
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="deploy-revision">Branch, tag or commit</Label>
          <Input
            id="deploy-revision"
            value={revision}
            onChange={(event) => onRevision(event.target.value)}
            placeholder="main"
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
            className="font-mono text-[13px]"
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="deploy-path">Directory</Label>
          <Input
            id="deploy-path"
            value={path}
            onChange={(event) => onPath(event.target.value)}
            placeholder="/"
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
            className="font-mono text-[13px]"
          />
        </div>
      </div>

      <div className="flex flex-col gap-1.5">
        {showToken ? (
          <>
            <Label htmlFor="deploy-token">
              GitHub token — only needed for private repositories
            </Label>
            <Input
              id="deploy-token"
              type="password"
              value={token}
              onChange={(event) => onToken(event.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={busy}
            />
            <p className="text-xs text-muted-foreground">
              Used once for this build, sent straight to the hosting engine and
              deleted after it; never stored here.
              {wasPrivate ? " Paste the token again." : ""}
            </p>
          </>
        ) : (
          <button
            type="button"
            onClick={() => onShowToken(true)}
            className="link self-start rounded-sm text-[13px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            Private repository
          </button>
        )}
      </div>
    </div>
  );
}

function UploadFields({
  busy,
  uploading,
  files,
  folderName,
  totalBytes,
  uploadMaxBytes,
  tooLarge,
  tooManyFiles,
  onPick,
}: {
  busy: boolean;
  uploading: boolean;
  files: number;
  folderName: string;
  totalBytes: number;
  uploadMaxBytes: number;
  tooLarge: boolean;
  tooManyFiles: boolean;
  onPick(event: ChangeEvent<HTMLInputElement>): void;
}) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="deploy-folder">Folder</Label>
        <Input
          id="deploy-folder"
          type="file"
          webkitdirectory=""
          multiple
          onChange={onPick}
          disabled={busy}
          className="h-auto py-1.5"
        />
        <p className="text-xs text-muted-foreground">
          .git, .env* and key files are skipped.
          {uploadMaxBytes > 0
            ? ` Up to ${formatBytes(uploadMaxBytes)} in total.`
            : ""}
        </p>
      </div>

      {files > 0 ? (
        <p className="text-ui text-subtle">
          {folderName ? (
            <span className="font-mono text-[13px]">{folderName}</span>
          ) : null}
          {folderName ? " · " : ""}
          {files} {plural(files, "file")} · {formatBytes(totalBytes)}
        </p>
      ) : null}

      {tooLarge ? (
        <p role="alert" className="text-xs text-destructive">
          That folder is {formatBytes(totalBytes)} — the control plane accepts up
          to {formatBytes(uploadMaxBytes)}.
        </p>
      ) : null}
      {tooManyFiles ? (
        <p role="alert" className="text-xs text-destructive">
          That folder has {files} files — the control plane accepts up to{" "}
          {maxUploadFiles}.
        </p>
      ) : null}

      {uploading ? (
        <div className="flex flex-col gap-1.5">
          {/* Indeterminate: the browser reports no progress for a fetch upload body. */}
          <Progress aria-label="Uploading the folder" />
          <p className="text-xs text-muted-foreground">
            Uploading {formatBytes(totalBytes)}…
          </p>
        </div>
      ) : null}
    </div>
  );
}
