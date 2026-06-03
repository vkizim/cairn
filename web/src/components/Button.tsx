import type { ButtonHTMLAttributes } from 'react'

type Variant = 'primary' | 'ghost'

const styles: Record<Variant, string> = {
  primary:
    'bg-slate-900 text-white hover:bg-slate-700 disabled:opacity-50 disabled:hover:bg-slate-900',
  ghost: 'bg-transparent text-slate-700 hover:bg-slate-200',
}

export function Button({
  variant = 'primary',
  className = '',
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
  return (
    <button
      className={`rounded-md px-3 py-1.5 text-sm font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400 ${styles[variant]} ${className}`}
      {...props}
    />
  )
}
