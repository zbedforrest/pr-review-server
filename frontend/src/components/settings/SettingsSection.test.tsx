import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsSection } from './SettingsSection';

const renderSection = (overrides: Partial<React.ComponentProps<typeof SettingsSection>> = {}) => {
  const props = {
    title: 'Review',
    dirty: true,
    canSave: true,
    saving: false,
    onSave: vi.fn(),
    onReset: vi.fn(),
    ...overrides,
  };
  render(
    <SettingsSection {...props}>
      <input aria-label="child control" />
    </SettingsSection>
  );
  return props;
};

const save = () => screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;
const reset = () => screen.getByRole('button', { name: 'Reset' }) as HTMLButtonElement;
const sectionElement = () => screen.getByRole('heading').closest('section') as HTMLElement;

describe('SettingsSection', () => {
  afterEach(cleanup);

  it('renders the title, description and children', () => {
    renderSection({ description: 'What gets reviewed' });
    expect(screen.getByRole('heading', { name: 'Review' })).toBeTruthy();
    expect(screen.getByText('What gets reviewed')).toBeTruthy();
    expect(screen.getByLabelText('child control')).toBeTruthy();
  });

  it('enables Save only when dirty, allowed and not saving', () => {
    renderSection();
    expect(save().disabled).toBe(false);
    cleanup();
    renderSection({ dirty: false });
    expect(save().disabled).toBe(true);
    cleanup();
    renderSection({ canSave: false });
    expect(save().disabled).toBe(true);
    cleanup();
    renderSection({ saving: true });
    expect(save().disabled).toBe(true);
  });

  it('enables Reset only when dirty', () => {
    renderSection({ canSave: false });
    expect(reset().disabled).toBe(false);
    cleanup();
    renderSection({ dirty: false });
    expect(reset().disabled).toBe(true);
  });

  it('calls onSave and onReset from their buttons', () => {
    const { onSave, onReset } = renderSection();
    fireEvent.click(save());
    fireEvent.click(reset());
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onReset).toHaveBeenCalledTimes(1);
  });

  it('announces Saving while pending and Saved afterwards in a status region', () => {
    renderSection();
    expect(screen.getByRole('status').textContent).toBe('');
    expect(sectionElement().getAttribute('aria-busy')).toBe('false');
    cleanup();
    renderSection({ saving: true });
    expect(screen.getByRole('status').textContent).toBe('Saving');
    expect(sectionElement().getAttribute('aria-busy')).toBe('true');
    cleanup();
    renderSection({ dirty: false, saved: true });
    expect(screen.getByRole('status').textContent).toBe('Saved');
    expect(sectionElement().getAttribute('aria-busy')).toBe('false');
  });

  it('shows the error in an alert only when one is given', () => {
    renderSection();
    expect(screen.queryByRole('alert')).toBeNull();
    cleanup();
    renderSection({ error: 'review_n_requests must be at least 1' });
    expect(screen.getByRole('alert').textContent).toBe('review_n_requests must be at least 1');
  });
});
