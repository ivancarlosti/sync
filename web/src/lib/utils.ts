import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

// cn merges conditional class names and resolves conflicting Tailwind utilities,
// which is what lets a shadcn-vue component accept a `class` prop that wins over
// its own defaults.
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
