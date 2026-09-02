"use client";

import {
  type ReactNode,
  useEffect,
  useState,
} from "react";
import {
  Activity,
  Check,
  ChevronRight,
  CircleAlert,
  Database,
  Globe2,
  LockKeyhole,
  Menu,
  MoreHorizontal,
  Pencil,
  Plus,
  RefreshCw,
  Server,
  ShieldCheck,
  Trash2,
  Waypoints,
} from "lucide-react";

import {
  RecordDialog,
  type RecordDraft,
  recordTypeName,
} from "@/components/record-dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Separator } from "@/components/ui/separator";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ZoneDialog } from "@/components/zone-dialog";
import {
  type Record as DNSRecord,
  type ServerStatus,
  type Zone,
} from "@/gen/dns/v1/dns_pb";
import { getDNSClient } from "@/lib/dns-client";
import { describeError } from "@/lib/errors";
import { lockOperatorSession } from "@/lib/operator-session";
import { cn } from "@/lib/utils";

type RecordEditor = { record?: DNSRecord };

type DeleteTarget =
  | { kind: "zone"; zone: Zone }
  | { kind: "record"; zone: Zone; record: DNSRecord };

type ClipboardError = {
  message: string;
  value: string;
};

type ZoneListProps = {
  zones: Zone[];
  selectedZoneID?: string;
  onSelect: (zoneID: string) => void;
  onCreate: () => void;
  afterSelect?: () => void;
};

function ZoneList({
  zones,
  selectedZoneID,
  onSelect,
  onCreate,
  afterSelect,
}: ZoneListProps) {
  return (
    <nav className="flex min-h-0 flex-1 flex-col" aria-label="DNS zones">
      <div className="flex items-center justify-between px-4 py-3">
        <p className="font-mono text-[0.68rem] font-medium tracking-[0.16em] text-muted-foreground uppercase">
          Zones
        </p>
        <span className="font-mono text-xs text-muted-foreground">
          {zones.length.toLocaleString()}
        </span>
      </div>
      <div className="min-h-0 flex-1 space-y-1 overflow-y-auto px-2 pb-3">
        {zones.map((zone) => {
          const selected = zone.id === selectedZoneID;
          return (
            <button
              key={zone.id}
              type="button"
              aria-current={selected ? "page" : undefined}
              className={cn(
                "group flex w-full items-center gap-2 rounded-md border border-transparent px-2 py-2.5 text-left transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
                selected
                  ? "border-primary/20 bg-primary/8 text-foreground"
                  : "text-muted-foreground hover:bg-muted/70 hover:text-foreground",
              )}
              onClick={() => {
                onSelect(zone.id);
                afterSelect?.();
              }}
            >
              <span
                className={cn(
                  "flex size-7 shrink-0 items-center justify-center rounded-md border bg-background",
                  selected ? "border-primary/30 text-primary" : "border-border",
                )}
              >
                <Globe2 className="size-3.5" aria-hidden="true" />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block truncate font-mono text-xs font-medium">
                  {zone.name}
                </span>
                <span className="mt-0.5 block text-[0.68rem] text-muted-foreground">
                  {zone.records.length} records
                </span>
              </span>
              <ChevronRight
                className={cn(
                  "size-3.5 shrink-0 transition-transform",
                  selected && "translate-x-0.5 text-primary",
                )}
                aria-hidden="true"
              />
            </button>
          );
        })}
      </div>
      <div className="border-t p-3">
        <Button className="w-full" variant="outline" onClick={onCreate}>
          <Plus data-icon="inline-start" />
          Add zone
        </Button>
      </div>
    </nav>
  );
}

function Stat({ icon, label, value }: { icon: ReactNode; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="text-muted-foreground" aria-hidden="true">
        {icon}
      </span>
      <span className="hidden text-xs text-muted-foreground sm:inline">
        {label}
      </span>
      <span className="font-mono text-xs font-medium tabular-nums">{value}</span>
    </div>
  );
}

function RecordBadge({ record }: { record: DNSRecord }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        "min-w-14 rounded-sm font-mono text-[0.66rem] tracking-wide",
        record.managed && "border-primary/25 bg-primary/8 text-primary",
      )}
    >
      {recordTypeName(record.type)}
    </Badge>
  );
}

function RecordActions({
  record,
  onEdit,
  onDelete,
}: {
  record: DNSRecord;
  onEdit: () => void;
  onDelete: () => void;
}) {
  if (record.managed) {
    return (
      <span className="inline-flex items-center gap-1 text-[0.68rem] text-muted-foreground">
        <ShieldCheck className="size-3.5 text-primary/80" aria-hidden="true" />
        Managed
      </span>
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${record.name} ${recordTypeName(record.type)} record`}>
          <MoreHorizontal aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-36">
        <DropdownMenuItem onSelect={onEdit}>
          <Pencil aria-hidden="true" />
          Edit
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" onSelect={onDelete}>
          <Trash2 aria-hidden="true" />
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function ConsoleSkeleton() {
  return (
    <div className="grid min-h-[calc(100vh-3.5rem)] lg:grid-cols-[17rem_1fr]">
      <aside className="hidden border-r bg-sidebar/80 p-3 lg:block">
        <Skeleton className="mb-4 h-4 w-16" />
        <div className="space-y-2">
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
        </div>
      </aside>
      <div className="p-4 sm:p-6 lg:p-8">
        <Skeleton className="h-7 w-52" />
        <Skeleton className="mt-3 h-4 w-72 max-w-full" />
        <Skeleton className="mt-8 h-24 w-full" />
        <Skeleton className="mt-5 h-72 w-full" />
      </div>
    </div>
  );
}

export function DNSConsole() {
  const [zones, setZones] = useState<Zone[]>([]);
  const [status, setStatus] = useState<ServerStatus>();
  const [selectedZoneID, setSelectedZoneID] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [apiError, setAPIError] = useState<string>();
  const [zoneDialogOpen, setZoneDialogOpen] = useState(false);
  const [recordEditor, setRecordEditor] = useState<RecordEditor>();
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget>();
  const [deleting, setDeleting] = useState(false);
  const [mobileZonesOpen, setMobileZonesOpen] = useState(false);
  const [copiedNameserver, setCopiedNameserver] = useState<string>();
  const [clipboardError, setClipboardError] = useState<ClipboardError>();

  async function refreshZones() {
    setRefreshing(true);
    try {
      const response = await getDNSClient().listZones({});
      setZones(response.zones);
      setStatus(response.status);
      setAPIError(undefined);
      setSelectedZoneID((current) =>
        current && response.zones.some((zone) => zone.id === current)
          ? current
          : response.zones[0]?.id,
      );
    } catch (caught) {
      setAPIError(describeError(caught));
    } finally {
      setRefreshing(false);
    }
  }

  useEffect(() => {
    let active = true;

    void getDNSClient()
      .listZones({})
      .then((response) => {
        if (!active) {
          return;
        }
        setZones(response.zones);
        setStatus(response.status);
        setAPIError(undefined);
        setSelectedZoneID(response.zones[0]?.id);
      })
      .catch((caught: unknown) => {
        if (active) {
          setAPIError(describeError(caught));
        }
      })
      .finally(() => {
        if (active) {
          setLoading(false);
        }
      });

    return () => {
      active = false;
    };
  }, []);

  const selectedZone = zones.find((zone) => zone.id === selectedZoneID);
  const recordCount = zones.reduce((count, zone) => count + zone.records.length, 0);

  function replaceZone(updated?: Zone) {
    if (!updated) {
      throw new Error("The server returned an empty zone response.");
    }
    setZones((current) =>
      current.map((zone) => (zone.id === updated.id ? updated : zone)),
    );
  }

  async function createZone(name: string) {
    const response = await getDNSClient().createZone({ name });
    if (!response.zone) {
      throw new Error("The server returned an empty zone response.");
    }
    const created = response.zone;
    setZones((current) =>
      [...current, created].sort((left, right) =>
        left.name.localeCompare(right.name),
      ),
    );
    setSelectedZoneID(created.id);
    setAPIError(undefined);
  }

  async function saveRecord(draft: RecordDraft) {
    if (!selectedZone) {
      throw new Error("Choose a zone before adding a record.");
    }

    const response = recordEditor?.record
      ? await getDNSClient().updateRecord({
          zoneId: selectedZone.id,
          recordId: recordEditor.record.id,
          ...draft,
        })
      : await getDNSClient().createRecord({
          zoneId: selectedZone.id,
          ...draft,
        });
    replaceZone(response.zone);
    setAPIError(undefined);
  }

  async function confirmDelete() {
    if (!deleteTarget) {
      return;
    }

    setDeleting(true);
    try {
      if (deleteTarget.kind === "zone") {
        await getDNSClient().deleteZone({ zoneId: deleteTarget.zone.id });
        const fallbackZoneID = zones.find(
          (zone) => zone.id !== deleteTarget.zone.id,
        )?.id;
        setZones((current) =>
          current.filter((zone) => zone.id !== deleteTarget.zone.id),
        );
        setSelectedZoneID((selected) =>
          selected === deleteTarget.zone.id ? fallbackZoneID : selected,
        );
      } else {
        const response = await getDNSClient().deleteRecord({
          zoneId: deleteTarget.zone.id,
          recordId: deleteTarget.record.id,
        });
        replaceZone(response.zone);
      }
      setAPIError(undefined);
      setDeleteTarget(undefined);
    } catch (caught) {
      setAPIError(describeError(caught));
      setDeleteTarget(undefined);
    } finally {
      setDeleting(false);
    }
  }

  async function copyNameserver(nameserver: string) {
    const value = nameserver.replace(/\.$/, "");
    try {
      await navigator.clipboard.writeText(value);
      setCopiedNameserver(nameserver);
      setClipboardError(undefined);
    } catch {
      setCopiedNameserver(undefined);
      setClipboardError({
        message: "Could not copy automatically. Copy this nameserver manually:",
        value,
      });
    }
  }

  return (
    <main className="min-h-screen bg-background/94">
      <header className="sticky top-0 z-40 flex h-14 items-center border-b bg-background/90 px-3 backdrop-blur-md sm:px-5">
        <div className="flex min-w-0 items-center gap-2.5">
          <div className="flex size-7 items-center justify-center rounded-md border border-primary/30 bg-primary/10 text-primary shadow-[0_0_24px_color-mix(in_oklch,var(--primary),transparent_78%)]">
            <Waypoints className="size-4" aria-hidden="true" />
          </div>
          <div className="min-w-0 leading-none">
            <p className="truncate text-sm font-semibold tracking-tight">Simple DNS</p>
            <p className="mt-1 hidden font-mono text-[0.6rem] tracking-[0.13em] text-muted-foreground uppercase sm:block">
              Authoritative control plane
            </p>
          </div>
        </div>

        <Separator orientation="vertical" className="mx-4 hidden h-5 sm:block" />

        <div className="hidden items-center gap-5 sm:flex">
          <Stat
            icon={<Globe2 className="size-3.5" />}
            label="zones"
            value={String(zones.length)}
          />
          <Stat
            icon={<Database className="size-3.5" />}
            label="records"
            value={String(recordCount)}
          />
          <Stat
            icon={<Activity className="size-3.5" />}
            label="queries"
            value={status ? status.queriesTotal.toLocaleString() : "0"}
          />
        </div>

        <div className="ml-auto flex items-center gap-2">
          <Badge
            variant="outline"
            className={cn(
              "hidden rounded-sm sm:inline-flex",
              apiError
                ? "border-destructive/35 text-destructive"
                : "border-primary/25 bg-primary/8 text-primary",
            )}
          >
            <span
              className={cn(
                "size-1.5 rounded-full",
                apiError ? "bg-destructive" : "bg-primary",
              )}
              aria-hidden="true"
            />
            {apiError ? "Control API unavailable" : "Control API online"}
          </Badge>
          <Button
            variant="ghost"
            aria-label="Lock operator console"
            onClick={() => lockOperatorSession("manual")}
          >
            <LockKeyhole aria-hidden="true" />
            <span className="hidden sm:inline">Lock</span>
          </Button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Refresh zones"
            onClick={() => void refreshZones()}
            disabled={refreshing}
          >
            <RefreshCw
              className={cn(refreshing && "animate-spin")}
              aria-hidden="true"
            />
          </Button>
          <Button onClick={() => setZoneDialogOpen(true)}>
            <Plus data-icon="inline-start" />
            <span className="hidden sm:inline">Add zone</span>
            <span className="sm:hidden">Zone</span>
          </Button>
        </div>
      </header>

      {loading ? (
        <ConsoleSkeleton />
      ) : (
        <div className="grid min-h-[calc(100vh-3.5rem)] lg:grid-cols-[17rem_minmax(0,1fr)]">
          <aside className="hidden min-h-0 border-r bg-sidebar/72 lg:flex">
            <ZoneList
              zones={zones}
              selectedZoneID={selectedZoneID}
              onSelect={setSelectedZoneID}
              onCreate={() => setZoneDialogOpen(true)}
            />
          </aside>

          <section className="min-w-0 p-3 sm:p-6 lg:p-8">
            <div className="mx-auto max-w-6xl">
              <div className="mb-4 flex items-center gap-2 lg:hidden">
                <Sheet open={mobileZonesOpen} onOpenChange={setMobileZonesOpen}>
                  <SheetTrigger asChild>
                    <Button variant="outline" className="min-w-0 max-w-full">
                      <Menu data-icon="inline-start" />
                      <span className="truncate font-mono">
                        {selectedZone?.name ?? "Choose zone"}
                      </span>
                    </Button>
                  </SheetTrigger>
                  <SheetContent side="left" className="w-[19rem] max-w-[88vw] bg-sidebar p-0">
                    <SheetHeader className="border-b">
                      <SheetTitle>DNS zones</SheetTitle>
                      <SheetDescription>
                        Choose an authoritative zone to manage.
                      </SheetDescription>
                    </SheetHeader>
                    <ZoneList
                      zones={zones}
                      selectedZoneID={selectedZoneID}
                      onSelect={setSelectedZoneID}
                      onCreate={() => {
                        setMobileZonesOpen(false);
                        setZoneDialogOpen(true);
                      }}
                      afterSelect={() => setMobileZonesOpen(false)}
                    />
                  </SheetContent>
                </Sheet>
              </div>

              {apiError ? (
                <Alert variant="destructive" className="mb-5 bg-destructive/5">
                  <CircleAlert aria-hidden="true" />
                  <AlertTitle>Control API request failed</AlertTitle>
                  <AlertDescription>
                    {apiError} Existing data remains visible; refresh to try again.
                  </AlertDescription>
                </Alert>
              ) : null}

              {!selectedZone ? (
                <div className="flex min-h-[30rem] items-center justify-center border border-dashed bg-card/60 p-6 text-center">
                  <div className="max-w-sm">
                    <div className="mx-auto flex size-12 items-center justify-center rounded-lg border border-primary/25 bg-primary/8 text-primary">
                      <Globe2 className="size-6" aria-hidden="true" />
                    </div>
                    <h1 className="mt-5 text-xl font-semibold tracking-tight">
                      Your first authoritative zone
                    </h1>
                    <p className="mt-2 text-sm leading-6 text-muted-foreground">
                      Add a domain, publish its records, then delegate it from
                      your registrar to the assigned nameservers.
                    </p>
                    <Button className="mt-5" onClick={() => setZoneDialogOpen(true)}>
                      <Plus data-icon="inline-start" />
                      Create a zone
                    </Button>
                  </div>
                </div>
              ) : (
                <>
                  <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <h1 className="truncate font-mono text-xl font-semibold tracking-tight sm:text-2xl">
                          {selectedZone.name}
                        </h1>
                        <Badge className="rounded-sm" variant="outline">
                          authoritative
                        </Badge>
                      </div>
                      <p className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                        <span className="font-mono tabular-nums">
                          serial {selectedZone.serial}
                        </span>
                        <span aria-hidden="true">·</span>
                        <span>{selectedZone.records.length} records</span>
                        <span aria-hidden="true">·</span>
                        <span>UDP + TCP</span>
                      </p>
                    </div>
                    <div className="flex items-center gap-2">
                      <Button onClick={() => setRecordEditor({})}>
                        <Plus data-icon="inline-start" />
                        Add record
                      </Button>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="outline" size="icon" aria-label="Zone actions">
                            <MoreHorizontal aria-hidden="true" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" className="w-40">
                          <DropdownMenuItem
                            variant="destructive"
                            onSelect={() =>
                              setDeleteTarget({ kind: "zone", zone: selectedZone })
                            }
                          >
                            <Trash2 aria-hidden="true" />
                            Delete zone
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </div>

                  <div className="mt-6 border bg-card/85">
                    <div className="flex items-center gap-3 border-b px-4 py-3">
                      <div className="flex size-8 shrink-0 items-center justify-center rounded-md border bg-background text-primary">
                        <Server className="size-4" aria-hidden="true" />
                      </div>
                      <div className="min-w-0">
                        <h2 className="text-sm font-medium">Nameservers</h2>
                        <p className="mt-0.5 text-xs text-muted-foreground">
                          Configure these at the domain registrar after the DNS
                          service has public addresses.
                        </p>
                      </div>
                    </div>
                    <div className="grid divide-y sm:grid-cols-2 sm:divide-x sm:divide-y-0">
                      {selectedZone.nameservers.map((nameserver) => (
                        <button
                          key={nameserver}
                          type="button"
                          className="flex items-center justify-between gap-3 px-4 py-3 text-left font-mono text-xs transition-colors hover:bg-muted/50 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring focus-visible:outline-none"
                          onClick={() => void copyNameserver(nameserver)}
                          aria-label={`Copy ${nameserver.replace(/\.$/, "")}`}
                        >
                          <span className="truncate">{nameserver.replace(/\.$/, "")}</span>
                          {copiedNameserver === nameserver ? (
                            <Check className="size-3.5 shrink-0 text-primary" aria-label="Copied" />
                          ) : (
                            <span className="shrink-0 font-sans text-[0.65rem] text-muted-foreground">
                              copy
                            </span>
                          )}
                        </button>
                      ))}
                    </div>
                    {clipboardError ? (
                      <p
                        role="alert"
                        className="border-t px-4 py-2.5 text-xs text-destructive"
                      >
                        {clipboardError.message}{" "}
                        <code className="select-all font-mono text-foreground">
                          {clipboardError.value}
                        </code>
                      </p>
                    ) : null}
                  </div>

                  <div className="mt-5 overflow-hidden border bg-card/85">
                    <div className="flex items-center justify-between border-b px-4 py-3">
                      <div>
                        <h2 className="text-sm font-medium">Zone records</h2>
                        <p className="mt-0.5 text-xs text-muted-foreground">
                          Changes publish to the in-memory serving snapshot immediately.
                        </p>
                      </div>
                      <Badge variant="secondary" className="rounded-sm font-mono tabular-nums">
                        {selectedZone.records.length}
                      </Badge>
                    </div>

                    <div className="hidden md:block">
                      <Table>
                        <TableHeader>
                          <TableRow className="hover:bg-transparent">
                            <TableHead className="w-28 px-4 text-[0.68rem] tracking-wide text-muted-foreground uppercase">
                              Type
                            </TableHead>
                            <TableHead className="text-[0.68rem] tracking-wide text-muted-foreground uppercase">
                              Name
                            </TableHead>
                            <TableHead className="text-[0.68rem] tracking-wide text-muted-foreground uppercase">
                              Value
                            </TableHead>
                            <TableHead className="w-24 text-right text-[0.68rem] tracking-wide text-muted-foreground uppercase">
                              TTL
                            </TableHead>
                            <TableHead className="w-24 pr-4 text-right">
                              <span className="sr-only">Actions</span>
                            </TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {selectedZone.records.map((record) => (
                            <TableRow key={record.id}>
                              <TableCell className="px-4">
                                <RecordBadge record={record} />
                              </TableCell>
                              <TableCell className="max-w-48 truncate font-mono text-xs">
                                {record.name}
                              </TableCell>
                              <TableCell className="max-w-80 truncate font-mono text-xs text-muted-foreground" title={record.value}>
                                {record.value}
                              </TableCell>
                              <TableCell className="text-right font-mono text-xs tabular-nums text-muted-foreground">
                                {record.ttl}s
                              </TableCell>
                              <TableCell className="pr-4 text-right">
                                <RecordActions
                                  record={record}
                                  onEdit={() => setRecordEditor({ record })}
                                  onDelete={() =>
                                    setDeleteTarget({
                                      kind: "record",
                                      zone: selectedZone,
                                      record,
                                    })
                                  }
                                />
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    </div>

                    <div className="divide-y md:hidden">
                      {selectedZone.records.map((record) => (
                        <div key={record.id} className="p-4">
                          <div className="flex items-center justify-between gap-3">
                            <div className="flex min-w-0 items-center gap-2">
                              <RecordBadge record={record} />
                              <span className="truncate font-mono text-xs font-medium">
                                {record.name}
                              </span>
                            </div>
                            <RecordActions
                              record={record}
                              onEdit={() => setRecordEditor({ record })}
                              onDelete={() =>
                                setDeleteTarget({
                                  kind: "record",
                                  zone: selectedZone,
                                  record,
                                })
                              }
                            />
                          </div>
                          <p className="mt-3 break-all font-mono text-xs leading-5 text-muted-foreground">
                            {record.value}
                          </p>
                          <p className="mt-2 font-mono text-[0.68rem] tabular-nums text-muted-foreground">
                            TTL {record.ttl}s
                          </p>
                        </div>
                      ))}
                    </div>
                  </div>

                  <p className="mt-4 flex items-center gap-2 text-[0.68rem] text-muted-foreground">
                    <ShieldCheck className="size-3.5 text-primary/70" aria-hidden="true" />
                    SOA and apex NS records are managed by the control plane.
                  </p>
                </>
              )}
            </div>
          </section>
        </div>
      )}

      {zoneDialogOpen ? (
        <ZoneDialog
          open
          onOpenChange={setZoneDialogOpen}
          onCreate={createZone}
        />
      ) : null}

      {recordEditor && selectedZone ? (
        <RecordDialog
          open
          zoneName={selectedZone.name}
          record={recordEditor.record}
          onOpenChange={(open) => {
            if (!open) {
              setRecordEditor(undefined);
            }
          }}
          onSubmit={saveRecord}
        />
      ) : null}

      <AlertDialog
        open={Boolean(deleteTarget)}
        onOpenChange={(open) => {
          if (!open && !deleting) {
            setDeleteTarget(undefined);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Delete {deleteTarget?.kind === "zone" ? "zone" : "record"}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget?.kind === "zone"
                ? `All records for ${deleteTarget.zone.name} will stop resolving from this service.`
                : `The ${deleteTarget ? recordTypeName(deleteTarget.record.type) : ""} record for ${deleteTarget?.record.name ?? "this name"} will stop resolving.`}
              {" "}This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(event) => {
                event.preventDefault();
                void confirmDelete();
              }}
            >
              {deleting ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </main>
  );
}
