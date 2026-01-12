import { useState, useRef, useEffect, memo } from 'react';
import { useUpdatePRNotes } from '@/hooks/usePRs';

interface NotesCellProps {
  owner: string;
  repo: string;
  number: number;
  initialNotes: string;
}

export const NotesCell = memo(function NotesCell({
  owner,
  repo,
  number,
  initialNotes
}: NotesCellProps) {
  const [isEditing, setIsEditing] = useState(false);
  const [notes, setNotes] = useState(initialNotes);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const updateNotesMutation = useUpdatePRNotes();

  // Focus input when entering edit mode
  useEffect(() => {
    if (isEditing && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [isEditing]);

  // Reset local state when initialNotes changes (from server)
  useEffect(() => {
    setNotes(initialNotes);
  }, [initialNotes]);

  const handleSave = async () => {
    if (notes === initialNotes) {
      setIsEditing(false);
      setError(null);
      return;
    }

    // Client-side validation
    if (notes.length > 15) {
      setError('Max 15 chars');
      return;
    }

    try {
      await updateNotesMutation.mutateAsync({
        owner,
        repo,
        number,
        notes: notes.trim(),
      });
      setIsEditing(false);
      setError(null);
    } catch (err) {
      setError('Save failed');
      // Keep in edit mode so user can retry
    }
  };

  const handleCancel = () => {
    setNotes(initialNotes);
    setIsEditing(false);
    setError(null);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      handleSave();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      handleCancel();
    }
  };

  const charCount = notes.length;
  const showWarning = charCount > 12;
  const showCounter = charCount > 10;

  if (isEditing) {
    return (
      <div className="notes-cell notes-cell--editing">
        <input
          ref={inputRef}
          type="text"
          className={`notes-cell__input ${showWarning ? 'notes-cell__input--warning' : ''}`}
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
          onBlur={handleSave}
          onKeyDown={handleKeyDown}
          maxLength={15}
          placeholder="Add note..."
        />
        {showCounter && (
          <span className={`notes-cell__counter ${showWarning ? 'notes-cell__counter--warning' : ''}`}>
            {charCount}/15
          </span>
        )}
        {error && <span className="notes-cell__error">{error}</span>}
      </div>
    );
  }

  return (
    <div
      className="notes-cell notes-cell--view"
      onClick={() => setIsEditing(true)}
      title="Click to edit notes"
    >
      {notes || <span className="notes-cell__placeholder">...</span>}
    </div>
  );
});
