---
paths:
  - "web/**"
---
# Frontend rules
- Vue 3 + TypeScript + Vite; Composition API with <script setup>; Pinia for state.
- Call the backend only through the generated API client; never hand-write request types.
- Format money from minor units with the currency's exponent; never do money maths in the browser beyond display.
- All user-facing text goes through i18n.
- Components get Vitest tests; key journeys get Playwright tests.
- Accessibility: labels on every input, keyboard navigation, sufficient contrast.
