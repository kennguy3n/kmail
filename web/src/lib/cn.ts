import { clsx } from "clsx";
import type { ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * `cn` — merge conditional class names and de-duplicate conflicting
 * Tailwind utilities. `clsx` resolves the conditional/array/object
 * syntax; `tailwind-merge` ensures the last conflicting utility wins
 * (e.g. `cn("p-2", "p-4")` → `"p-4"`).
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
