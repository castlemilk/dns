"use client";

import { LockKeyhole, MoreVertical, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import type { Record as DNSRecord, Zone } from "@/gen/dns/v1/dns_pb";
import { recordTypeName } from "@/lib/dns-values";
import { recordOrigin } from "@/lib/zone-model";

export type RecordRowActionsProps = {
  recordId: string;
  zone: Zone;
  onEdit: (record: DNSRecord) => void;
  onDelete: (record: DNSRecord) => void;
};

/**
 * The record is looked up by id in the current zone at click time: a REPLACE import
 * regenerates every record id, so a captured record object can be stale.
 */
export function RecordRowActions({
  recordId,
  zone,
  onEdit,
  onDelete,
}: RecordRowActionsProps) {
  const record = zone.records.find((candidate) => candidate.id === recordId);
  if (!record) {
    return null;
  }

  const origin = recordOrigin(record);
  if (origin !== "user") {
    // The reconcilers rewrite these within one pass, so the control plane refuses hand
    // edits outright (§3.2). The tooltip names the one action that releases them.
    const engine = origin === "hosting" ? "Website" : "Email";
    const release =
      origin === "hosting" ? "detach the site to edit" : "unbind email to edit";
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            className="inline-flex rounded-sm text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <LockKeyhole aria-hidden="true" className="size-4" />
            <span className="sr-only">
              Written by the {engine} engine — {release}
            </span>
          </span>
        </TooltipTrigger>
        <TooltipContent>
          Written by the {engine} engine — {release}
        </TooltipContent>
      </Tooltip>
    );
  }

  if (record.managed) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            className="inline-flex rounded-sm text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <ShieldCheck aria-hidden="true" className="size-4" />
            <span className="sr-only">Managed by the control plane</span>
          </span>
        </TooltipTrigger>
        <TooltipContent>Managed by the control plane</TooltipContent>
      </Tooltip>
    );
  }

  const resolve = () =>
    zone.records.find((candidate) => candidate.id === recordId);

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={`Actions for ${record.name} ${recordTypeName(record.type)} record`}
        >
          <MoreVertical aria-hidden="true" className="size-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem
          onSelect={() => {
            const current = resolve();
            if (current) {
              onEdit(current);
            }
          }}
        >
          Edit
        </DropdownMenuItem>
        <DropdownMenuItem
          variant="destructive"
          onSelect={() => {
            const current = resolve();
            if (current) {
              onDelete(current);
            }
          }}
        >
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
