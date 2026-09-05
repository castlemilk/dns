import "react";

/**
 * `@types/react` 19.2 does not declare the directory-picker attribute, and the deploy
 * dialog's "drop a folder" input needs it (`<input type="file" webkitdirectory />`).
 * Declaring it here keeps the one non-standard attribute in a single reviewed place
 * instead of a cast at the call site.
 */
declare module "react" {
  // The type parameter must be named exactly as in React's own declaration for the
  // interfaces to merge, even though nothing here uses it.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface InputHTMLAttributes<T> {
    webkitdirectory?: "" | boolean;
  }
}
