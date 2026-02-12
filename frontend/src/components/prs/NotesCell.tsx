import { useState, useCallback, useRef, useEffect } from 'react';
import { useUpdatePRNotes } from '@/hooks/usePRs';
import { useTelemetry } from '@/hooks/useTelemetry';
import './NotesCell.scss';

interface NotesCellProps {
  owner: string;
  repo: string;
  number: number;
  notes: string;
}

export function NotesCell({ owner, repo, number, notes }: NotesCellProps) {
  const [isEditing, setIsEditing] = useState(false);
  const [editValue, setEditValue] = useState(notes);
  const inputRef = useRef<HTMLInputElement>(null);
  const updateNotes = useUpdatePRNotes();
  const { track } = useTelemetry();

  // Focus input when entering edit mode
  useEffect(() => {
    if (isEditing && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [isEditing]);

  const handleStartEditing = useCallback(() => {
    setEditValue(notes); // Sync from prop when starting to edit
    setIsEditing(true);
  }, [notes]);

  const handleSave = useCallback(() => {
    const trimmedValue = editValue.trim().slice(0, 15);
    if (trimmedValue !== notes) {
      track('edit_notes', { pr_owner: owner, pr_repo: repo, pr_number: number });
      updateNotes.mutate({
        owner,
        repo,
        number,
        notes: trimmedValue,
      });
    }
    setIsEditing(false);
  }, [owner, repo, number, notes, editValue, updateNotes, track]);

  const handleCancel = useCallback(() => {
    setEditValue(notes);
    setIsEditing(false);
  }, [notes]);

  const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      handleSave();
    } else if (e.key === 'Escape') {
      handleCancel();
    }
  }, [handleSave, handleCancel]);

  if (isEditing) {
    return (
      <div className="notes-cell notes-cell--editing">
        <input
          ref={inputRef}
          type="text"
          value={editValue}
          onChange={(e) => setEditValue(e.target.value.slice(0, 15))}
          onKeyDown={handleKeyDown}
          onBlur={handleSave}
          maxLength={15}
          placeholder="Add note..."
          className="notes-cell__input"
        />
      </div>
    );
  }

  return (
    <div
      className={`notes-cell ${notes ? 'notes-cell--has-content' : 'notes-cell--empty'}`}
      onClick={handleStartEditing}
      title={notes || 'Click to add note'}
    >
      {notes || <span className="notes-cell__placeholder">-</span>}
    </div>
  );
}
