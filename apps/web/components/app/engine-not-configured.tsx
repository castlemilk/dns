import Link from "next/link";

import {
  engineNotConfiguredCopy,
  type EngineName,
} from "@/lib/platform-model";

export type EngineNotConfiguredProps = {
  engine: EngineName;
  /** Environment variable NAMES the control API is missing. Never values. */
  missingEnv: string[];
};

/**
 * The honest empty state for an engine this deployment does not run: what it is, which
 * variables an operator has to set, and nothing else. No price, no sample numbers,
 * no "Soon" — the route exists and says exactly why it is empty.
 */
export function EngineNotConfigured({
  engine,
  missingEnv,
}: EngineNotConfiguredProps) {
  const { title, body, names } = engineNotConfiguredCopy(engine, missingEnv);

  return (
    <section className="rounded-[10px] border border-dashed border-line-strong px-6 py-12 text-center">
      <h2 className="text-base font-semibold">{title}</h2>
      <p className="text-ui mx-auto mt-2 max-w-[46ch] text-muted-foreground">
        {body}
      </p>
      {names.length ? (
        <ul className="mt-4 flex flex-wrap justify-center gap-2">
          {names.map((name) => (
            <li
              key={name}
              className="rounded-md bg-fill px-2 py-1 font-mono text-[12.5px] text-soft"
            >
              {name}
            </li>
          ))}
        </ul>
      ) : null}
      <p className="mt-5 text-[13px]">
        <Link href="/settings" className="link">
          Settings →
        </Link>
      </p>
    </section>
  );
}
