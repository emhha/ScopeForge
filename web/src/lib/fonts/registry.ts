// Preserve the existing font choices and their local fallback families.
const geist = { variable: "--font-geist" };
const inter = { variable: "--font-inter" };
const notoSans = { variable: "--font-noto-sans" };
const nunitoSans = { variable: "--font-nunito-sans" };
const figtree = { variable: "--font-figtree" };
const roboto = { variable: "--font-roboto" };
const raleway = { variable: "--font-raleway" };
const dmSans = { variable: "--font-dm-sans" };
const publicSans = { variable: "--font-public-sans" };
const outfit = { variable: "--font-outfit" };
const geistMono = { variable: "--font-geist-mono" };
const jetBrainsMono = { variable: "--font-jetbrains-mono" };
const notoSerif = { variable: "--font-noto-serif" };
const robotoSlab = { variable: "--font-roboto-slab" };
const merriweather = { variable: "--font-merriweather" };
const lora = { variable: "--font-lora" };
const playfairDisplay = { variable: "--font-playfair-display" };

export const fontRegistry = {
  geist: { label: "Geist", font: geist },
  inter: { label: "Inter", font: inter },
  notoSans: { label: "Noto Sans", font: notoSans },
  nunitoSans: { label: "Nunito Sans", font: nunitoSans },
  figtree: { label: "Figtree", font: figtree },
  roboto: { label: "Roboto", font: roboto },
  raleway: { label: "Raleway", font: raleway },
  dmSans: { label: "DM Sans", font: dmSans },
  publicSans: { label: "Public Sans", font: publicSans },
  outfit: { label: "Outfit", font: outfit },
  geistMono: { label: "Geist Mono", font: geistMono },
  geistPixelSquare: { label: "Geist Pixel Square", font: { variable: "--font-geist-pixel-square" } },
  jetBrainsMono: { label: "JetBrains Mono", font: jetBrainsMono },
  notoSerif: { label: "Noto Serif", font: notoSerif },
  robotoSlab: { label: "Roboto Slab", font: robotoSlab },
  merriweather: { label: "Merriweather", font: merriweather },
  lora: { label: "Lora", font: lora },
  playfairDisplay: { label: "Playfair Display", font: playfairDisplay },
} as const;

export type FontKey = keyof typeof fontRegistry;

export const fontOptions = (Object.entries(fontRegistry) as [FontKey, { label: string; font: { variable: string } }][]).map(
  ([key, f]) => ({
    key,
    label: f.label,
    variable: f.font.variable,
  }),
);
