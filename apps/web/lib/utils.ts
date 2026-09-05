import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

/**
 * tailwind-merge only knows Tailwind's stock scales. The design adds custom font-size
 * steps (`text-2xs` / `text-label` / `text-ui`, from --text-* in globals.css) and custom
 * text colours (`text-subtle`, `text-warning`, …, from --color-* in globals.css). Without
 * this extension every one of those names falls into the same "text-color" group, so
 * cn("text-ui", "text-subtle") silently drops the font size and the element reverts to
 * 16px. Registering the two sets in their real groups keeps a size and a colour together.
 */
const twMerge = extendTailwindMerge({
  extend: {
    classGroups: {
      "font-size": [{ text: ["2xs", "label", "ui"] }],
      "text-color": [
        {
          text: [
            "subtle",
            "soft",
            "light",
            "faint",
            "link",
            "link-hover",
            "success",
            "warning",
            "page",
          ],
        },
      ],
    },
  },
});

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
