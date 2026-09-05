"use client";

import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { useMailQueue } from "@/hooks/use-platform-resources";
import { toDate } from "@/lib/format";

export type MailQueueSheetProps = {
  zoneId: string;
  open: boolean;
  onOpenChange(open: boolean): void;
};

/**
 * What the mail server has in flight for this domain right now. The queue is the only
 * delivery statistic this edition exposes per domain; when the engine could not answer
 * completely the counts are withheld rather than guessed.
 */
export function MailQueueSheet({
  zoneId,
  open,
  onOpenChange,
}: MailQueueSheetProps) {
  // The empty zone id disables the request: a closed sheet must not query the queue.
  const { queue, loaded, error, refresh } = useMailQueue(open ? zoneId : "");

  if (!open) {
    return null;
  }

  const summary = queue?.summary;
  const messages = queue?.messages ?? [];
  // What is queued may only be stated when this read of the queue actually landed:
  // an empty `messages` is the initial value while the request is in flight, is what
  // is left behind when it failed, and says nothing at all when the mail server told
  // us it could not read the queue completely.
  const queueKnown = loaded && !error && summary !== undefined && summary.available;

  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="w-[92vw] data-[side=right]:sm:max-w-2xl"
      >
        <SheetHeader>
          <SheetTitle>Mail queue</SheetTitle>
          <SheetDescription>
            Messages the mail server has not delivered yet.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-4 pb-4">
          {error ? (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          ) : null}

          {!loaded ? (
            <div className="flex flex-col gap-1.5">
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          ) : error ? (
            // The alert above is the whole statement: counts from an earlier read
            // would be presented as the queue right now, which is not what is known.
            null
          ) : !summary ? (
            <p role="status" className="text-ui text-muted-foreground">
              The mail server answered without a queue summary, so nothing about
              the queue is shown.
            </p>
          ) : !summary.available ? (
            <p role="status" className="text-ui text-muted-foreground">
              The queue couldn&apos;t be read completely; counts are not shown.
            </p>
          ) : (
            // Refresh replaces these counts in place; role="status" is what makes the
            // new ones reach a screen reader.
            <p role="status" className="text-ui text-subtle">
              {summary.scheduled} scheduled · {summary.failing} failing ·{" "}
              {summary.failed} failed
              {summary.observedAt ? (
                <span className="text-muted-foreground">
                  {" "}
                  · <RelativeTime date={toDate(summary.observedAt)} />
                </span>
              ) : null}
            </p>
          )}

          {!queueKnown ? null : messages.length === 0 ? (
            <p className="text-ui text-muted-foreground">Nothing waiting.</p>
          ) : (
            <div
              role="table"
              aria-label="Queued messages"
              className="overflow-hidden rounded-[10px] border border-line"
            >
              <div role="rowgroup">
                <div
                  role="row"
                  className="eyebrow hidden grid-cols-[1.2fr_1.4fr_1fr_90px] border-b border-line bg-fill-faint px-4 py-2.5 md:grid"
                >
                  <div role="columnheader">From</div>
                  <div role="columnheader">To</div>
                  <div role="columnheader">Status</div>
                  <div role="columnheader" className="text-right">
                    Attempts
                  </div>
                </div>
              </div>
              <div role="rowgroup">
                {messages.map((message) => (
                  <div
                    key={message.id}
                    role="row"
                    className="text-ui grid grid-cols-1 gap-1 border-b border-line-soft px-4 py-3 last:border-0 md:grid-cols-[1.2fr_1.4fr_1fr_90px] md:items-center md:gap-0"
                  >
                    <div
                      role="cell"
                      className="font-mono text-[13px] break-all text-subtle"
                    >
                      {message.from || "—"}
                    </div>
                    <div
                      role="cell"
                      className="font-mono text-[13px] break-all text-subtle"
                    >
                      {message.to.join(", ") || "—"}
                    </div>
                    <div role="cell" className="text-muted-foreground">
                      {message.status || "—"}
                      {message.queue ? ` · ${message.queue}` : ""}
                      {message.dueAt ? (
                        <>
                          {" · due "}
                          <RelativeTime date={toDate(message.dueAt)} />
                        </>
                      ) : null}
                    </div>
                    <div
                      role="cell"
                      className="font-mono text-[13px] text-muted-foreground md:text-right"
                    >
                      {message.attempts}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          <Button
            variant="outline"
            size="sm"
            className="self-start"
            onClick={() => {
              void refresh();
            }}
          >
            Refresh
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}
