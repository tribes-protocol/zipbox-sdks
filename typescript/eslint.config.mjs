// The published SDK's own lint config. It does not use the monorepo's lucy rules:
// those enforce @tribes-terminal/* helpers, and a package published to npm cannot
// depend on private workspace packages.
import js from '@eslint/js'
import typescriptEslint from '@typescript-eslint/eslint-plugin'
import typescriptParser from '@typescript-eslint/parser'
import prettierConfig from 'eslint-config-prettier'

export default [
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**'] },
  js.configs.recommended,
  {
    files: ['**/*.ts'],
    languageOptions: {
      parser: typescriptParser,
      parserOptions: {
        project: ['./tsconfig.build.json', './tsconfig.vitest.json'],
        tsconfigRootDir: import.meta.dirname
      }
    },
    plugins: { '@typescript-eslint': typescriptEslint },
    rules: {
      ...typescriptEslint.configs['strict-type-checked'].rules,
      ...typescriptEslint.configs['stylistic-type-checked'].rules,
      'no-undef': 'off',
      'no-unused-vars': 'off',
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/consistent-type-assertions': ['error', { assertionStyle: 'never' }],
      '@typescript-eslint/restrict-template-expressions': ['error', { allowNumber: true }],
      '@typescript-eslint/explicit-function-return-type': ['error', { allowExpressions: true }]
    }
  },
  prettierConfig
]
