"use client"

import * as React from "react"

/**
 * Radix returns focus to `<DialogTrigger>` when a modal closes: its own
 * `onCloseAutoFocus` calls `preventDefault()` (cancelling the focus scope's restore)
 * and then focuses `context.triggerRef.current`. Every dialog and sheet in this console
 * is controlled by an `open` prop and opened from an ordinary button, a card link or a
 * dropdown item — none of them is a `DialogTrigger` — so that ref is null, the restore
 * is cancelled and nothing takes its place. Escape closed the dialog and dropped the
 * keyboard on `document.body`: the next Tab started again from the top of the page.
 *
 * So remember the element that had focus when the content first rendered (Radix has not
 * moved focus yet at that point) and put the keyboard back there on close.
 */

/**
 * The last control the reader actually used outside any transient overlay. This is the
 * fallback for when the element that opened the dialog is not somewhere to put the
 * keyboard back — a dropdown menu item, which is still in the document once the menu
 * shuts (Radix keeps it mounted) but is invisible and unreachable.
 *
 * It is recorded from `pointerdown`, `click` and `keydown` as well as `focusin`, because a
 * Radix dropdown trigger calls `preventDefault()` on its own pointerdown to stop the
 * browser focusing it — so a kebab opened with the mouse never fires a focus event at
 * all, and a focus-only recorder would miss the one control the reader actually pressed.
 */
let lastControlOutsideOverlay: HTMLElement | null = null

/** Surfaces whose contents are never a place to hand the keyboard back to. */
const overlaySurfaces =
  "[data-slot=dialog-content],[data-slot=sheet-content],[data-slot=alert-dialog-content]," +
  "[role=menu],[role=listbox],[role=dialog],[role=alertdialog]"

const controls = "button,a[href],summary,[role=button],input,select,textarea"

function recordControl(event: Event): void {
  const target = event.target as HTMLElement | null
  if (!target || typeof target.closest !== "function") {
    return
  }
  const control = target.closest<HTMLElement>(controls) ?? target
  if (control.closest(overlaySurfaces)) {
    return
  }
  lastControlOutsideOverlay = control
}

if (typeof document !== "undefined") {
  for (const type of ["focusin", "pointerdown", "keydown", "click"]) {
    document.addEventListener(type, recordControl, true)
  }
}

/**
 * Connected is not enough. A Radix menu that has been dismissed keeps its items in the
 * document (`data-state="closed"`, laid out, still measurable), so `isConnected` and a
 * client rect both say yes about an item nobody can see. A menu item is never the right
 * place to hand the keyboard back to anyway: the menu's own trigger is.
 *
 * Deliberately not `[data-state=closed]` — a dropdown TRIGGER carries that attribute
 * whenever its menu is shut, and the trigger is exactly where the keyboard should land.
 */
const focusable = (element: HTMLElement | null): element is HTMLElement =>
  element !== null &&
  element !== document.body &&
  element.isConnected &&
  typeof element.focus === "function" &&
  element.getClientRects().length > 0 &&
  element.closest("[role=menu],[role=listbox]") === null

export type ReturnFocusHandler = (event: Event) => void

/**
 * Builds the `onCloseAutoFocus` handler a Radix content element needs to hand the
 * keyboard back. `consumerHandler` is whatever the caller passed; it runs first and a
 * caller that prevents the default keeps full control.
 */
export function useReturnFocus(
  consumerHandler?: (event: Event) => void
): ReturnFocusHandler {
  // Captured during the first render of the content, which happens before Radix's focus
  // scope moves focus into it — so this is the element the reader came from.
  const [opener] = React.useState<HTMLElement | null>(() =>
    typeof document === "undefined"
      ? null
      : (document.activeElement as HTMLElement | null)
  )

  return React.useCallback(
    (event: Event) => {
      consumerHandler?.(event)
      if (event.defaultPrevented) {
        return
      }
      const target = focusable(opener)
        ? opener
        : focusable(lastControlOutsideOverlay)
          ? lastControlOutsideOverlay
          : null
      if (!target) {
        // Nothing sensible to focus: let Radix's own handler run.
        return
      }
      // Cancels Radix's "focus the (missing) trigger" handler, which composeEventHandlers
      // skips once the event's default is prevented.
      event.preventDefault()
      target.focus()
    },
    [consumerHandler, opener]
  )
}
