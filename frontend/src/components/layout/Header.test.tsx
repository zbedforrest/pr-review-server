import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Header } from './Header';

vi.mock('./GenerateReviewForm', () => ({
  GenerateReviewForm: () => <div>Generate Review Form</div>,
}));

vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => ({ track: vi.fn() }),
}));

describe('Header', () => {
  afterEach(() => cleanup());

  it('links to the settings page for everyone', () => {
    render(<Header />);
    expect(screen.getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('/settings');
    expect(screen.getByRole('link', { name: 'Stats' }).getAttribute('href')).toBe('/usage-stats');
  });
});
